package codexcli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

const pendingToolTTL = 5 * time.Minute

type codexTurnState struct {
	server            *appServer
	release           func(bool)
	requestDir        string
	threadID          string
	turnID            string
	model             string
	shapeSignature    string
	allowedImageViews map[string]struct{}
	pending           map[string]json.RawMessage
	timer             *time.Timer
	claimed           bool
	cleanupOnce       sync.Once
}

func (s *codexTurnState) cleanup(healthy bool) {
	s.cleanupOnce.Do(func() {
		if s.timer != nil {
			s.timer.Stop()
		}
		_ = os.RemoveAll(s.requestDir)
		s.release(healthy)
	})
}

func requestShapeSignature(model, system string, tools []map[string]any, policy protocol.ToolPolicy) string {
	raw, _ := json.Marshal(map[string]any{
		"model":  model,
		"system": system,
		"tools":  tools,
		"policy": policy,
	})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (p *Provider) claimPending(key, signature string, req *protocol.Request) (*codexTurnState, map[string]dynamicToolResult, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil, false, nil
	}
	results, err := requestToolResults(req)
	if err != nil {
		return nil, nil, false, err
	}

	p.sessionMu.Lock()
	state := p.sessions[key]
	if state == nil {
		p.sessionMu.Unlock()
		return nil, nil, false, nil
	}
	if state.claimed {
		p.sessionMu.Unlock()
		return nil, nil, true, errors.New("the previous Codex tool turn for this Claude agent is still being resumed")
	}
	if state.shapeSignature != signature {
		delete(p.sessions, key)
		p.sessionMu.Unlock()
		state.cleanup(false)
		return nil, nil, false, nil
	}
	for callID := range state.pending {
		if _, ok := results[callID]; !ok {
			p.sessionMu.Unlock()
			return nil, nil, true, fmt.Errorf("Claude did not return a tool_result for pending Codex call %q", callID)
		}
	}
	state.claimed = true
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	p.sessionMu.Unlock()
	return state, results, true, nil
}

func (p *Provider) parkPending(key string, state *codexTurnState) error {
	key = strings.TrimSpace(key)
	if key == "" || len(state.pending) == 0 {
		return errors.New("cannot preserve a Codex tool turn without a Claude session key and pending tool calls")
	}
	p.sessionMu.Lock()
	if p.sessionsClosed {
		p.sessionMu.Unlock()
		return errors.New("Codex provider is closed")
	}
	previous := p.sessions[key]
	p.sessions[key] = state
	state.claimed = false
	state.timer = time.AfterFunc(pendingToolTTL, func() { p.expirePending(key, state) })
	p.sessionMu.Unlock()
	if previous != nil && previous != state {
		previous.cleanup(false)
	}
	return nil
}

func (p *Provider) finishPending(key string, state *codexTurnState, healthy bool) {
	key = strings.TrimSpace(key)
	if key != "" {
		p.sessionMu.Lock()
		if p.sessions[key] == state {
			delete(p.sessions, key)
		}
		p.sessionMu.Unlock()
	}
	state.cleanup(healthy)
}

func (p *Provider) expirePending(key string, state *codexTurnState) {
	p.sessionMu.Lock()
	if p.sessions[key] != state || state.claimed {
		p.sessionMu.Unlock()
		return
	}
	delete(p.sessions, key)
	p.sessionMu.Unlock()
	state.cleanup(false)
}

func (p *Provider) closePending() {
	p.sessionMu.Lock()
	p.sessionsClosed = true
	states := make([]*codexTurnState, 0, len(p.sessions))
	for _, state := range p.sessions {
		states = append(states, state)
	}
	p.sessions = make(map[string]*codexTurnState)
	p.sessionMu.Unlock()
	for _, state := range states {
		state.cleanup(false)
	}
}

type dynamicToolResult struct {
	success      bool
	contentItems []map[string]any
}

func requestToolResults(req *protocol.Request) (map[string]dynamicToolResult, error) {
	results := make(map[string]dynamicToolResult)
	for _, message := range req.Messages {
		blocks, err := protocol.DecodeBlocks(message.Content)
		if err != nil {
			return nil, err
		}
		for _, block := range blocks {
			if block.Type != "tool_result" || strings.TrimSpace(block.ToolUseID) == "" {
				continue
			}
			items, err := dynamicToolContent(block.Content)
			if err != nil {
				return nil, fmt.Errorf("decode tool_result %q: %w", block.ToolUseID, err)
			}
			results[block.ToolUseID] = dynamicToolResult{success: !block.IsError, contentItems: items}
		}
	}
	return results, nil
}

func dynamicToolContent(raw json.RawMessage) ([]map[string]any, error) {
	blocks, err := protocol.DecodeBlocks(raw)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			items = append(items, map[string]any{"type": "inputText", "text": block.Text})
		case "image":
			if block.Source == nil {
				continue
			}
			imageURL := strings.TrimSpace(block.Source.URL)
			if block.Source.Type == "base64" {
				mediaType := strings.TrimSpace(block.Source.MediaType)
				if mediaType == "" {
					mediaType = "image/png"
				}
				if _, err := base64.StdEncoding.DecodeString(block.Source.Data); err != nil {
					return nil, fmt.Errorf("invalid base64 image: %w", err)
				}
				imageURL = "data:" + mediaType + ";base64," + block.Source.Data
			}
			if imageURL != "" {
				items = append(items, map[string]any{"type": "inputImage", "imageUrl": imageURL})
			}
		default:
			encoded := block.Raw
			if len(encoded) == 0 {
				encoded, _ = json.Marshal(block)
			}
			items = append(items, map[string]any{"type": "inputText", "text": string(encoded)})
		}
	}
	if len(items) == 0 {
		items = append(items, map[string]any{"type": "inputText", "text": ""})
	}
	return items, nil
}

func (s *codexTurnState) answerPending(results map[string]dynamicToolResult) error {
	for callID, responseID := range s.pending {
		result := results[callID]
		if err := s.server.rpc.respond(responseID, map[string]any{
			"success":      result.success,
			"contentItems": result.contentItems,
		}); err != nil {
			return err
		}
	}
	s.pending = make(map[string]json.RawMessage)
	return nil
}
