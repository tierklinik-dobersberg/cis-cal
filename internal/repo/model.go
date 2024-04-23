package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/api/calendar/v3"
)

var ErrInvalidEvent = errors.New("invalid event")

type Calendar struct {
	ID       string
	Name     string
	Timezone string
	Location *time.Location
	Color    string
}

type Event struct {
	ID           string
	Summary      string
	Description  string
	StartTime    time.Time
	EndTime      *time.Time
	CalendarID   string
	FullDayEvent bool
	Data         *StructuredEvent
}

type StructuredEvent struct {
	CustomerSource    string
	CustomerID        string
	AnimalID          []string
	CreatedBy         string
	RequiredResources []string
}

type EventSearchOptions struct {
	FromTime *time.Time
	ToTime   *time.Time
	EventID  *string
}

func (s *EventSearchOptions) From(t time.Time) *EventSearchOptions {
	s.FromTime = &t

	return s
}

func (s *EventSearchOptions) To(t time.Time) *EventSearchOptions {
	s.ToTime = &t

	return s
}

func (s *EventSearchOptions) ForDay(t time.Time) *EventSearchOptions {
	s.From(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()))
	s.To(time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location()))

	return s
}

func WithEventsAfter(after time.Time) SearchOption {
	return func(eso *EventSearchOptions) {
		eso.FromTime = &after
	}
}

func WithEventsBefore(before time.Time) SearchOption {
	return func(eso *EventSearchOptions) {
		eso.ToTime = &before
	}
}

func WithEventId(id string) SearchOption {
	return func(eso *EventSearchOptions) {
		eso.EventID = &id
	}
}

func googleEventToModel(_ context.Context, calid string, item *calendar.Event) (*Event, error) {
	var (
		err   error
		start time.Time
		end   *time.Time
	)

	if item == nil {
		return nil, fmt.Errorf("%w: received nil item", ErrInvalidEvent)
	}

	if item.Start == nil {
		logrus.WithFields(logrus.Fields{
			"event": item,
		}).Errorf("failed to process google calendar event: event.Start == nil")

		return nil, fmt.Errorf("%w: event with ID %s does not have start time", ErrInvalidEvent, item.Id)
	}

	if item.Start.DateTime != "" {
		start, err = time.Parse(time.RFC3339, item.Start.DateTime)
	} else {
		start, err = time.Parse("2006-01-02", item.Start.Date)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to parse event start time: %w", err)
	}

	if !item.EndTimeUnspecified {
		var t time.Time
		if item.End.DateTime != "" {
			t, err = time.Parse(time.RFC3339, item.End.DateTime)
		} else {
			t, err = time.Parse("2006-01-02", item.End.Date)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to parse event end time: %w", err)
		}
		end = &t
	}

	newDescription, data, err := parseDescription(item.Description)
	if err != nil {
		logrus.Errorf("failed to parse calendar event meta data: %s", err)
	}
	if err == nil {
		item.Description = newDescription
	}

	return &Event{
		ID:           item.Id,
		Summary:      strings.TrimSpace(item.Summary),
		Description:  strings.TrimSpace(item.Description),
		StartTime:    start,
		EndTime:      end,
		FullDayEvent: item.Start.DateTime == "" && item.Start.Date != "",
		CalendarID:   calid,
		Data:         data,
	}, nil
}

func parseDescription(desc string) (string, *StructuredEvent, error) {
	allLines := strings.Split(desc, "\n")
	var (
		sectionLines      []string
		strippedDescr     string
		foundSectionStart bool
	)
	for idx, line := range allLines {
		line := strings.TrimSpace(line)
		if line == "[CIS]" {
			foundSectionStart = true
			sectionLines = allLines[idx+1:]
			strippedDescr = strings.TrimSpace(strings.Join(allLines[:idx], "\n"))

			break
		}
	}
	if !foundSectionStart {
		return "", nil, nil
	}

	reader := strings.NewReader(strings.Join(sectionLines, "\n"))

	dec := json.NewDecoder(reader)

	var data StructuredEvent
	if err := dec.Decode(&data); err != nil {
		return "", nil, err
	}

	return strippedDescr, &data, nil
}
