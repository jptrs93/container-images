package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Settings  map[string]any `yaml:"settings"`
	HBA       []string       `yaml:"hba"`
	Roles     []Role         `yaml:"roles"`
	Databases []Database     `yaml:"databases"`
}

type ValueSource struct {
	Value string `yaml:"value"`
	Env   string `yaml:"env"`
}

type Role struct {
	Name        ValueSource  `yaml:"name"`
	Permissions []Permission `yaml:"permissions"`
}

type Permission struct {
	Database string   `yaml:"database"`
	Schema   string   `yaml:"schema"`
	Grants   []string `yaml:"grants"`
}

type Database struct {
	Name       string   `yaml:"name"`
	Owner      string   `yaml:"owner"`
	Schemas    []string `yaml:"schemas"`
	Extensions []string `yaml:"extensions"`
}

func Load(path string) (Config, error) {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func (source ValueSource) Resolve(field string) (string, error) {
	if source.Value != "" && source.Env != "" {
		return "", fmt.Errorf("%s cannot set both value and env", field)
	}
	if source.Env != "" {
		value := os.Getenv(source.Env)
		if value == "" {
			return "", fmt.Errorf("%s environment variable %s is empty", field, source.Env)
		}
		return value, nil
	}
	if source.Value == "" {
		return "", fmt.Errorf("%s must set value or env", field)
	}
	return source.Value, nil
}
