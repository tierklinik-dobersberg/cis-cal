package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

type Config struct {
	CredentialsFile   string   `json:"credentialsFile"`
	TokenFile         string   `json:"tokenFile"`
	IgnoreCalendars   []string `json:"ignoreCalendars"`
	IdmURL            string   `json:"idmUrl"`
	EventsServiceUrl  string   `json:"eventsServiceUrl"`
	AllowedOrigins    []string `json:"allowedOrigins"`
	ListenAddress     string   `json:"listen"`
	DefaultCountry    string   `json:"defaultCountry"`
	MongoURL          string   `json:"mongoURL"`
	MongoDatabaseName string   `json:"database"`
	FreeSlots         struct {
		IgnoreShiftTags []string `json:"ignoreShiftTags"`
		RosterTypeName  string   `json:"rosterTypeName"`
	} `json:"freeSlots"`
}

// LoadConfig loads the configuration file from cfgPath.
func LoadConfig(cfgPath string) (Config, error) {
	content, err := os.ReadFile(cfgPath)
	if err != nil {
		return Config{}, err
	}

	switch filepath.Ext(cfgPath) {
	case ".yml", ".yaml":
		content, err = yaml.YAMLToJSON(content)
		if err != nil {
			return Config{}, err
		}

	case ".json":
		// nothing to do here
	default:
		return Config{}, fmt.Errorf("unsupported file format %q", filepath.Ext(cfgPath))
	}

	dec := json.NewDecoder(bytes.NewReader(content))
	dec.DisallowUnknownFields()

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return cfg, err
	}

	if cfg.ListenAddress == "" {
		cfg.ListenAddress = ":8080"
	}

	if cfg.IdmURL == "" {
		cfg.IdmURL = os.Getenv("IDM_URL")
	}

	if cfg.DefaultCountry == "" {
		cfg.DefaultCountry = "AT"
	}

	return cfg, nil
}
