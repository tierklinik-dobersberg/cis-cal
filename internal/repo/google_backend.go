package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/sirupsen/logrus"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/events/v1/eventsv1connect"
	"github.com/tierklinik-dobersberg/apis/pkg/cli"
	"github.com/tierklinik-dobersberg/cis-cal/internal/config"
	"github.com/tierklinik-dobersberg/cis/pkg/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/singleflight"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type SearchOption func(*EventSearchOptions)

// Service allows to read and manipulate google
// calendar events.
type Service interface {
	ListCalendars(ctx context.Context) ([]Calendar, error)
	ListEvents(ctx context.Context, calendarID string, filter ...SearchOption) ([]Event, error)
	LoadEvent(ctx context.Context, calendarID string, eventID string, ignoreCache bool) (*Event, error)
	CreateEvent(ctx context.Context, calID, name, description string, startTime time.Time, duration time.Duration, data *StructuredEvent) (*Event, error)
	DeleteEvent(ctx context.Context, calID, eventID string) error
	MoveEvent(ctx context.Context, originCalendarId, eventId, targetCalendarId string) (event *Event, err error)
	UpdateEvent(ctx context.Context, event Event) (*Event, error)
}

type googleCalendarBackend struct {
	*calendar.Service
	EventsClient    eventsv1connect.EventServiceClient
	location        *time.Location
	ignoreCalendars []string

	cacheLock   sync.Mutex
	eventsCache map[string]*googleEventCache
	loadGroup   singleflight.Group
}

// New creates a new calendar service from cfg.
func New(ctx context.Context, cfg config.Config) (Service, error) {
	creds, err := credsFromFile(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read credentials file %s: %w", cfg.CredentialsFile, err)
	}

	token, err := tokenFromFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read token from %s: %w", cfg.TokenFile, err)
	}

	client := creds.Client(ctx, token)
	calSvc, err := calendar.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("failed to create calendar client: %w", err)
	}

	svc := &googleCalendarBackend{
		Service:         calSvc,
		eventsCache:     make(map[string]*googleEventCache),
		location:        time.UTC,
		ignoreCalendars: cfg.IgnoreCalendars,
		EventsClient:    eventsv1connect.NewEventServiceClient(cli.NewInsecureHttp2Client(), cfg.EventsServiceUrl),
	}
	if svc.location == nil {
		svc.location = time.Local
	}

	// create a new eventCache for each calendar right now
	if _, err := svc.ListCalendars(ctx); err != nil {
		logrus.Errorf("failed to start watching calendars: %s", err)
	}

	return svc, nil
}

// Authenticate retrieves a new token and saves it under TokenFile.
func Authenticate(cfg config.Config) error {
	creds, err := credsFromFile(cfg.CredentialsFile)
	if err != nil {
		return fmt.Errorf("failed reading %s: %w", cfg.CredentialsFile, err)
	}

	token, err := getTokenFromWeb(creds)
	if err != nil {
		return err
	}

	if err := saveTokenFile(token, cfg.TokenFile); err != nil {
		return err
	}

	return nil
}

func (svc *googleCalendarBackend) ListCalendars(ctx context.Context) ([]Calendar, error) {
	res, err := svc.Service.CalendarList.List().ShowHidden(true).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve list of calendars: %w", err)
	}

	var list = make([]Calendar, 0, len(res.Items))
	for _, item := range res.Items {
		loc, err := time.LoadLocation(item.TimeZone)
		if err != nil {
			logrus.Errorf("failed to parse timezone %s from calendar %s", item.TimeZone, item.Id)
		}

		// check if the calendar should be ingored based on IngoreCalendar=
		if svc.shouldIngore(item) {
			continue
		}

		list = append(list, Calendar{
			ID:       item.Id,
			Name:     item.Summary,
			Timezone: item.TimeZone,
			Location: loc,
			Color:    item.BackgroundColor,
		})
		// immediately prepare the calendar cache
		if _, err = svc.cacheFor(ctx, item.Id); err != nil {
			logrus.Errorf("failed to perpare calendar event cache for %s: %s", item.Id, err)
		}
	}

	return list, nil
}

func (svc *googleCalendarBackend) ListEvents(ctx context.Context, calendarID string, searchOpts ...SearchOption) ([]Event, error) {
	opts := new(EventSearchOptions)

	for _, fn := range searchOpts {
		fn(opts)
	}

	cache, err := svc.cacheFor(ctx, calendarID)
	if err != nil {
		logrus.Errorf("failed to get event cache for calendar %s: %s", calendarID, err)
	}

	if cache != nil {
		events, ok := cache.tryLoadFromCache(ctx, opts)
		if ok {
			return events, nil
		}
		logrus.Debugf("cache miss when loading events for %s", calendarID)
	}

	return svc.loadEvents(ctx, calendarID, opts)
}

func (svc *googleCalendarBackend) CreateEvent(ctx context.Context, calID, name, description string, startTime time.Time, duration time.Duration, data *StructuredEvent) (*Event, error) {
	ctx, sp := otel.Tracer("").Start(ctx, "google.backend#CreateEvent")
	defer sp.End()

	sp.SetAttributes(
		attribute.String("calendar.id", calID),
		attribute.String("calendar.name", name),
		attribute.String("calendar.description", description),
		attribute.String("calendar.start_time", startTime.String()),
		attribute.String("calendar.duration", duration.String()),
	)

	// convert structured event data to it's string representation
	// and append to description.
	if data != nil {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)

		if err := enc.Encode(data); err != nil {
			return nil, err
		}

		description = strings.TrimSpace(description) + "\n\n[CIS]\n" + buf.String()
	}

	res, err := svc.Service.Events.Insert(calID, &calendar.Event{
		Summary:     name,
		Description: description,
		Start: &calendar.EventDateTime{
			DateTime: startTime.Format(time.RFC3339),
		},
		End: &calendar.EventDateTime{
			DateTime: startTime.Add(duration).Format(time.RFC3339),
		},
		Status: "confirmed",
	}).Context(ctx).Do()
	if err != nil {
		trace.RecordAndLog(ctx, err)

		return nil, fmt.Errorf("failed to insert event upstream: %w", err)
	}
	logrus.Infof("created event with id %s", res.Id)

	if cache, _ := svc.cacheFor(ctx, calID); cache != nil {
		cache.triggerSync()
	}

	return googleEventToModel(ctx, calID, res)
}

func (svc *googleCalendarBackend) UpdateEvent(ctx context.Context, event Event) (*Event, error) {
	evt, err := svc.Service.Events.Update(event.CalendarID, event.ID, &calendar.Event{
		Summary:     event.Summary,
		Description: event.Description,
		Start: &calendar.EventDateTime{
			DateTime: event.StartTime.Format(time.RFC3339),
		},
		End: &calendar.EventDateTime{
			DateTime: event.EndTime.Format(time.RFC3339),
		},
		Status: "confirmed",
	}).Context(ctx).Do()

	if err != nil {
		return nil, err
	}

	if cache, err := svc.cacheFor(ctx, event.CalendarID); err == nil && cache != nil {
		cache.triggerSync()
	} else {
		logrus.Errorf("[update] failed to trigger sync for event calendar id %q: %s", event.CalendarID, err)
	}

	return googleEventToModel(ctx, event.CalendarID, evt)
}

func (svc *googleCalendarBackend) MoveEvent(ctx context.Context, originCalendarId string, eventId string, targetCalendarId string) (*Event, error) {
	result, err := svc.Service.Events.Move(originCalendarId, eventId, targetCalendarId).Context(ctx).Do()
	if err != nil {
		return nil, err
	}

	if cache, err := svc.cacheFor(ctx, originCalendarId); err == nil && cache != nil {
		cache.triggerSync()
	} else {
		logrus.Errorf("[move] failed to trigger sync for origin calendar id %q: %s", originCalendarId, err)
	}

	if cache, err := svc.cacheFor(ctx, targetCalendarId); err == nil && cache != nil {
		cache.triggerSync()
	} else {
		logrus.Errorf("[move] failed to trigger sync for target calendar id %q: %s", targetCalendarId, err)
	}

	return googleEventToModel(ctx, targetCalendarId, result)
}

func (svc *googleCalendarBackend) DeleteEvent(ctx context.Context, calID, eventID string) error {
	err := svc.Service.Events.Delete(calID, eventID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to delete event upstream: %w", err)
	}

	cache, err := svc.cacheFor(ctx, calID)
	if err == nil {
		cache.triggerSync()
	}

	return nil
}

func (svc *googleCalendarBackend) cacheFor(ctx context.Context, calID string) (*googleEventCache, error) {
	svc.cacheLock.Lock()
	defer svc.cacheLock.Unlock()

	cache, ok := svc.eventsCache[calID]
	if ok {
		logrus.Debugf("using existing event cache for %s", calID)

		return cache, nil
	}

	cache, err := newCache(ctx, calID, svc.location, svc.Service, svc.EventsClient)
	if err != nil {
		return nil, err
	}

	svc.eventsCache[calID] = cache
	logrus.Debugf("created new event cache for calendar %s", calID)

	return cache, nil
}

func (svc *googleCalendarBackend) LoadEvent(ctx context.Context, calendarID, eventID string, ignoreCache bool) (*Event, error) {
	opts := &EventSearchOptions{
		EventID: &eventID,
	}
	if !ignoreCache {
		if cache, err := svc.cacheFor(ctx, calendarID); err == nil && cache != nil {
			events, ok := cache.tryLoadFromCache(ctx, opts)
			if ok && len(events) == 1 {
				return &events[0], nil
			}
		}
	}

	evt, err := svc.Service.Events.Get(calendarID, eventID).Context(ctx).Do()
	if err != nil {
		var googleError *googleapi.Error
		if errors.As(err, &googleError) {
			switch googleError.Code {
			case http.StatusNotFound, http.StatusGone:
				return nil, connect.NewError(connect.CodeNotFound, googleError)
			}
		}

		return nil, err
	}

	return googleEventToModel(ctx, calendarID, evt)
}

// trunk-ignore(golangci-lint/cyclop)
func (svc *googleCalendarBackend) loadEvents(ctx context.Context, calendarID string, searchOpts *EventSearchOptions) ([]Event, error) {
	call := svc.Events.List(calendarID).ShowDeleted(false).SingleEvents(true)

	key := calendarID
	if searchOpts != nil {
		if searchOpts.FromTime != nil {
			call = call.TimeMin(searchOpts.FromTime.Format(time.RFC3339))
			key += fmt.Sprintf("-%s", searchOpts.FromTime.Format(time.RFC3339))
		}
		if searchOpts.ToTime != nil {
			call = call.TimeMax(searchOpts.ToTime.Format(time.RFC3339))
			key += fmt.Sprintf("-%s", searchOpts.ToTime.Format(time.RFC3339))
		}

		if searchOpts.EventID != nil {
			key += "-" + *searchOpts.EventID
		}
	}

	res, err, shared := svc.loadGroup.Do(key, func() (interface{}, error) {
		var events []Event
		var pageToken string
		for {
			if pageToken != "" {
				call.PageToken(pageToken)
			}
			res, err := call.Context(ctx).Do()
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve page from upstream: %w", err)
			}

			for _, item := range res.Items {
				evt, err := googleEventToModel(ctx, calendarID, item)

				if err != nil {
					logrus.Errorf(err.Error())

					continue
				}

				// if we're searching for a single event ID, we can check for that ID and
				// exit early
				if searchOpts.EventID != nil {
					if evt.ID == *searchOpts.EventID {
						return []Event{*evt}, nil
					}
				} else {
					events = append(events, *evt)
				}

			}

			if res.NextPageToken != "" {
				pageToken = res.NextPageToken

				continue
			}

			break
		}

		return events, nil
	})

	svc.loadGroup.Forget(key)
	if shared {
		logrus.Infof("shared calendar load between multiple callers")
	}

	// trunk-ignore(golangci-lint/forcetypeassert)
	return res.([]Event), err
}

func (svc *googleCalendarBackend) shouldIngore(item *calendar.CalendarListEntry) bool {
	for _, value := range svc.ignoreCalendars {
		if item.Id == value {
			return true
		}
	}

	return false
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var token oauth2.Token
	if err := json.Unmarshal(content, &token); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON token: %w", err)
	}

	return &token, nil
}

func saveTokenFile(token *oauth2.Token, path string) error {
	blob, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON token: %w", err)
	}

	return ioutil.WriteFile(path, blob, 0600)
}

func credsFromFile(path string) (*oauth2.Config, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	config, err := google.ConfigFromJSON(content, calendar.CalendarScope, "https://www.googleapis.com/auth/userinfo.profile")
	if err != nil {
		return nil, fmt.Errorf("failed to get configuration from JSON: %w", err)
	}

	return config, nil
}

func getTokenFromWeb(config *oauth2.Config) (*oauth2.Token, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+ //nolint:forbidigo
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		return nil, fmt.Errorf("unable to read authorization code: %w", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve token: %w", err)
	}

	return tok, nil
}
