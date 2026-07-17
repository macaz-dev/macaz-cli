package secrets

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

const service = "github.com/macaz-dev/macaz-cli"

const (
	OpenAIAPIKey     = "openai-api-key"
	OpenRouterAPIKey = "openrouter-api-key"
	AnthropicAPIKey  = "anthropic-api-key"
	OpenAIAccount    = "openai-subscription"
)

func Set(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("secret cannot be empty")
	}
	if err := keyring.Set(service, name, value); err != nil {
		return fmt.Errorf("store %s in the operating-system credential store: %w", name, err)
	}
	return nil
}

func Get(name, environment string) (string, error) {
	if environment != "" {
		if value := strings.TrimSpace(os.Getenv(environment)); value != "" {
			return value, nil
		}
	}
	value, err := keyring.Get(service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		label := environment
		if label == "" {
			label = name
		}
		return "", fmt.Errorf("%s is not configured; run `macaz provider set`", label)
	}
	if err != nil {
		return "", fmt.Errorf("read %s from the operating-system credential store: %w", name, err)
	}
	if strings.TrimSpace(value) == "" {
		label := environment
		if label == "" {
			label = name
		}
		return "", fmt.Errorf("%s is empty; run `macaz provider set`", label)
	}
	return value, nil
}

func Configured(name, environment string) (bool, error) {
	if environment != "" && strings.TrimSpace(os.Getenv(environment)) != "" {
		return true, nil
	}
	value, err := keyring.Get(service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s from the operating-system credential store: %w", name, err)
	}
	return strings.TrimSpace(value) != "", nil
}

func Delete(name string) error {
	err := keyring.Delete(service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s from the operating-system credential store: %w", name, err)
	}
	return nil
}

func DeleteAll() error {
	var joined error
	for _, name := range []string{OpenAIAPIKey, OpenRouterAPIKey, AnthropicAPIKey, OpenAIAccount} {
		joined = errors.Join(joined, Delete(name))
	}
	return joined
}
