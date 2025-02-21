package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	IsFree       bool
	CreateTime   time.Time

	CustomerAnnotation *calendarv1.CustomerAnnotation
}

type EventList []Event

func (el EventList) Len() int { return len(el) }
func (el EventList) Less(i, j int) bool {
	if el[i].StartTime.Before(el[j].StartTime) {
		return true
	}

	return el[i].EndTime.Before(*el[j].EndTime)
}
func (el EventList) Swap(i, j int) {
	el[i], el[j] = el[j], el[i]
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

	var createTime time.Time
	if item.Created != "" {
		var err error

		createTime, err = time.Parse(time.RFC3339, item.Created)
		if err != nil {
			// log the error but continue since it's not that important to have the create time.
			slog.Error("failed to parse calendar event create time", "error", err, "time", item.Created)
		}
	}

	var ca *calendarv1.CustomerAnnotation
	if item.ExtendedProperties != nil && len(item.ExtendedProperties.Shared) > 0 {
		if value, ok := item.ExtendedProperties.Shared["tkd.calendar.v1.CustomerAnnoation"]; ok {
			ca = new(calendarv1.CustomerAnnotation)

			if err := protojson.Unmarshal([]byte(value), ca); err != nil {
				slog.Error("failed to unmarshal customer annoation", "error", err)
				ca = nil
			}
		}
	}

	return &Event{
		ID:                 item.Id,
		Summary:            strings.TrimSpace(item.Summary),
		Description:        strings.TrimSpace(item.Description),
		StartTime:          start,
		EndTime:            end,
		FullDayEvent:       item.Start.DateTime == "" && item.Start.Date != "",
		CalendarID:         calid,
		CreateTime:         createTime,
		CustomerAnnotation: ca,
	}, nil
}

func (model *Event) ToProto() (*calendarv1.CalendarEvent, error) {
	var endTime *timestamppb.Timestamp
	var any *anypb.Any
	var err error

	if model.EndTime != nil {
		endTime = timestamppb.New(*model.EndTime)
	}

	if model.CustomerAnnotation != nil {
		any, err = anypb.New(model.CustomerAnnotation)
		if err != nil {
			return nil, err
		}
	}

	var createTime *timestamppb.Timestamp

	if !model.CreateTime.IsZero() {
		createTime = timestamppb.New(model.CreateTime)
	}

	return &calendarv1.CalendarEvent{
		Id:          model.ID,
		CalendarId:  model.CalendarID,
		StartTime:   timestamppb.New(model.StartTime),
		EndTime:     endTime,
		FullDay:     model.FullDayEvent,
		ExtraData:   any,
		Summary:     model.Summary,
		Description: model.Description,
		IsFree:      model.IsFree,
		CreateTime:  createTime,
	}, nil

}
