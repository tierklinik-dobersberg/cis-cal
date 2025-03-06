package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Config holds the configuration for the calendar service.
type Config struct {
	// CredentialsFile holds the path to the credentials file
	// required to access the Google Calendar API.
	CredentialsFile string `json:"credentialsFile"`

	// TokenFile holds the path to the token file required to
	// access Google Calendar API.
	TokenFile string `json:"tokenFile"`

	// IgnoreCalendars is a list of Google Calendar IDs that should
	// be ignored.
	IgnoreCalendars []string `json:"ignoreCalendars"`

	// IdmUrl holds the path to the IDM service.
	IdmURL string `json:"idmUrl"`

	// EventsServiceUrl holds the path to the events service.
	EventsServiceUrl string `json:"eventsServiceUrl"`

	// AllowedOrigins configures allowed origins for CORS requests.
	AllowedOrigins []string `json:"allowedOrigins"`

	// ListenAddress is the address ([host]:port) at which the calender service
	// should listen and serve the Connect-RPC Service.
	ListenAddress string `json:"listen"`

	// DefaultCountry is the default country to use.
	DefaultCountry string `json:"defaultCountry"`

	// MongoURL holds the path to the Mongo-DB database.
	MongoURL string `json:"mongoURL"`

	// MongoDatabaseName is the name of the mongodb database
	MongoDatabaseName string `json:"database"`

	// FreeSlots holds configuration for calculating free-slots.
	// This requires access to the rosterd service.
	FreeSlots struct {
		// IngoreShiftTags can be set to a list of tags. Workshifts with this tag
		// will be ignored.
		IgnoreShiftTags []string `json:"ignoreShiftTags"`

		// RosterTypeName is the name of the roster-type that should be considered
		// when calculating free slots.
		RosterTypeName string `json:"rosterTypeName"`
	} `json:"freeSlots"`

	// ICals can be used to add additional, read-only ical calendars.
	ICals []ICalConfig `json:"ical"`
}

type ICalConfig struct {
	// Name is the name of the external ical calendar.
	Name string `json:"name"`

	// Color might be used to specify a specific color for this calendar.
	Color string `json:"color"`

	// URLS holds one or more iCal URLS. Events from those calendars will
	// be merged into a single virtual calendar.
	URLS []string `json:"urls"`

	// Hidden might be set to true to exclude this calendar from requests that
	// do not explicitly specify the calendar name.
	Hidden bool `json:"hidden"`

	// PollInterval returns the polling interval for the calendar.
	PollInterval string `json:"pollingInterval"`
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
