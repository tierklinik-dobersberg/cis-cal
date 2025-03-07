package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrInvalidEvent = errors.New("invalid event")
	ErrNotFound     = errors.New("event not found")
	ErrReadOnly     = errors.New("calendar is readonly")
)

type SearchOption func(*EventSearchOptions)

type Reader interface {
	ListCalendars(ctx context.Context) ([]Calendar, error)
	ListEvents(ctx context.Context, calendarID string, filter ...SearchOption) ([]Event, error)
	LoadEvent(ctx context.Context, calendarID string, eventID string, ignoreCache bool) (*Event, error)
}

type Writer interface {
	CreateEvent(ctx context.Context, calID, name, description string, startTime time.Time, duration time.Duration, resources []string, data *calendarv1.CustomerAnnotation) (*Event, error)
	DeleteEvent(ctx context.Context, calID, eventID string) error
	MoveEvent(ctx context.Context, originCalendarId, eventId, targetCalendarId string) (event *Event, err error)
	UpdateEvent(ctx context.Context, event Event) (*Event, error)
}

// ReadWriter allows to read and manipulate google
// calendar events.
type ReadWriter interface {
	Reader
	Writer
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
	Resources    []string

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

func (s *EventSearchOptions) String() string {
	var str []string
	if s.FromTime != nil {
		str = append(str, fmt.Sprintf("from=%s", s.FromTime.Format(time.RFC3339)))
	}

	if s.ToTime != nil {
		str = append(str, fmt.Sprintf("to=%s", s.ToTime.Format(time.RFC3339)))
	}

	if s.EventID != nil {
		str = append(str, fmt.Sprintf("id=%s", *s.EventID))
	}

	return strings.Join(str, " ")
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
		Resources:   model.Resources,
	}, nil
}
