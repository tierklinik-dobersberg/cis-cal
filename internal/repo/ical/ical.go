package ical

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	ical "github.com/arran4/golang-ical"
	"github.com/tierklinik-dobersberg/cis-cal/internal/config"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
)

var (
	ErrCalendarExists = errors.New("calendar already registered")
)

type Repository struct {
	calendarLock sync.RWMutex
	calendars    []config.ICalConfig

	eventsLock sync.RWMutex
	events     map[string][]repo.Event

	triggerRefresh chan struct{}
	wg             sync.WaitGroup
}

func New() *Repository {
	return &Repository{
		triggerRefresh: make(chan struct{}),
		events:         make(map[string][]repo.Event),
	}
}

func (r *Repository) Wait() {
	r.wg.Wait()
}

func (r *Repository) Start(ctx context.Context) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		ticker := time.NewTicker(time.Minute * 5)
		defer ticker.Stop()

		lastUpdates := make(map[string]time.Time)

		for {
			c, cancel := context.WithTimeout(ctx, time.Minute*4)

			r.update(c, lastUpdates)
			cancel()

			select {
			case <-ticker.C:
			case <-r.triggerRefresh:
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (r *Repository) Add(cfg config.ICalConfig, triggerRefresh bool) error {
	r.calendarLock.Lock()
	defer r.calendarLock.Unlock()

	for _, c := range r.calendars {
		if c.Name == cfg.Name {
			return ErrCalendarExists
		}
	}

	r.calendars = append(r.calendars, cfg)

	// trigger a refresh
	if triggerRefresh {
		r.triggerRefresh <- struct{}{}
	}

	return nil
}

func (r *Repository) update(ctx context.Context, lastUpdates map[string]time.Time) {

	events := make(map[string][]repo.Event)
	for _, cfg := range r.GetCalendars() {
		last, ok := lastUpdates[cfg.Name]

		if ok {
			// verify if we should update the calendar again
			interval := time.Minute * 10

			if cfg.PollInterval != "" {
				d, err := time.ParseDuration(cfg.PollInterval)
				if err != nil {
					slog.Error("invalid duration for ical calendar", "name", cfg.Name, "error", err)
					continue
				}

				interval = d
			}

			if last.Add(interval).After(time.Now()) {
				// skip this calendar
				continue
			}
		}

		slog.Info("updating virtual calendar", "name", cfg.Name)

		for _, url := range cfg.URLS {
			calendar, err := ical.ParseCalendarFromUrl(url, ctx)

			if err != nil {
				slog.Error("failed to fetch ical calendar URL", "url", url, "name", cfg.Name, "error", err)
				continue
			}

			for _, e := range calendar.Events() {
				var (
					summary     string
					description string
				)

				if summaryProp := e.GetProperty(ical.ComponentPropertySummary); summaryProp != nil {
					summary = summaryProp.Value
				}

				if descProp := e.GetProperty(ical.ComponentPropertyDescription); descProp != nil && descProp.Value != "" {
					description = descProp.Value
				}

				start, err := e.GetStartAt()
				if err != nil {
					slog.Error("failed to get ical event start time", "url", url, "name", cfg.Name, "error", err, "id", e.Id())
					continue
				}

				var endTime *time.Time
				end, err := e.GetEndAt()
				if err != nil {
					slog.Error("failed to get ical event end time", "url", url, "name", cfg.Name, "error", err, "id", e.Id())
				}
				if !end.IsZero() {
					endTime = &end
				}

				converted := repo.Event{
					CalendarID:   cfg.Name,
					ID:           e.Id(),
					Summary:      summary,
					Description:  description,
					StartTime:    start,
					EndTime:      endTime,
					FullDayEvent: false,
					IsFree:       false,
				}

				events[cfg.Name] = append(events[cfg.Name], converted)
			}

		}

		slog.Info("loaded events for virtual ical calendar", "name", cfg.Name, "count", len(events[cfg.Name]))
	}

	r.eventsLock.Lock()
	defer r.eventsLock.Unlock()

	r.events = events
}

func (r *Repository) GetCalendars() []config.ICalConfig {
	r.calendarLock.RLock()
	defer r.calendarLock.RUnlock()

	return slices.Clone(r.calendars)
}

func (r *Repository) GetEvents() map[string][]repo.Event {
	r.eventsLock.RLock()
	defer r.eventsLock.RUnlock()

	return maps.Clone(r.events)
}

func (r *Repository) ListCalendars(ctx context.Context) ([]repo.Calendar, error) {
	cals := r.GetCalendars()

	result := make([]repo.Calendar, len(cals))

	for idx, c := range cals {
		result[idx] = repo.Calendar{
			ID:       c.Name,
			Name:     c.Name,
			Timezone: time.Local.String(),
			Color:    c.Color,
			Readonly: true,
			Reader:   r,
			Hidden:   c.Hidden,
		}
	}

	return result, nil
}

func (r *Repository) exists(id string) error {
	r.calendarLock.Lock()
	defer r.calendarLock.Unlock()

	for _, c := range r.calendars {
		if c.Name == id {
			return nil
		}
	}

	return repo.ErrNotFound
}

func (r *Repository) ListEvents(ctx context.Context, calId string, opts ...repo.SearchOption) ([]repo.Event, error) {
	if err := r.exists(calId); err != nil {
		return nil, err
	}

	search := new(repo.EventSearchOptions)
	for _, o := range opts {
		o(search)
	}
	slog.Info("searching for ical events", "filter", search.String())

	r.eventsLock.RLock()
	defer r.eventsLock.RUnlock()

	all := slices.Clone(r.events[calId])
	events := make([]repo.Event, 0, len(all))

	for _, evt := range all {
		if !repo.EventMatches(evt, search) {
			continue
		}

		events = append(events, evt)
	}

	return events, nil
}

func (r *Repository) LoadEvent(ctx context.Context, calId string, eventId string, _ bool) (*repo.Event, error) {
	if err := r.exists(calId); err != nil {
		return nil, err
	}

	r.eventsLock.RLock()
	defer r.eventsLock.RUnlock()

	for _, e := range r.events[calId] {
		if e.ID == eventId {
			return &e, nil
		}
	}

	return nil, repo.ErrNotFound
}
