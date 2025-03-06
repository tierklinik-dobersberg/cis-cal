package repo

import (
	"context"
	"time"

	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
)

type Calendar struct {
	ID       string
	Name     string
	Timezone string
	Color    string
	Readonly bool
	Hidden   bool

	Reader
}

func (c *Calendar) Writer() (Writer, error) {
	if c.Readonly {
		return nil, ErrReadOnly
	}

	if w, ok := c.Reader.(Writer); ok {
		return w, nil
	}

	return nil, ErrReadOnly
}

func (c Calendar) ToProto() *calendarv1.Calendar {
	return &calendarv1.Calendar{
		Id:       c.ID,
		Name:     c.Name,
		Timezone: c.Timezone,
		Color:    c.Color,
		Readonly: c.Readonly,
	}
}

func (c Calendar) ListEvents(ctx context.Context, filter ...SearchOption) ([]Event, error) {
	return c.Reader.ListEvents(ctx, c.ID, filter...)
}

func (c Calendar) LoadEvent(ctx context.Context, eventID string, ignoreCache bool) (*Event, error) {
	return c.Reader.LoadEvent(ctx, c.ID, eventID, ignoreCache)
}

func (c Calendar) CreateEvent(ctx context.Context, name, description string, startTime time.Time, duration time.Duration, resources []string, data *calendarv1.CustomerAnnotation) (*Event, error) {
	w, err := c.Writer()
	if err != nil {
		return nil, err
	}

	return w.CreateEvent(ctx, c.ID, name, description, startTime, duration, resources, data)
}

func (c Calendar) DeleteEvent(ctx context.Context, eventID string) error {
	w, err := c.Writer()
	if err != nil {
		return err
	}

	return w.DeleteEvent(ctx, c.ID, eventID)
}

func (c Calendar) MoveEvent(ctx context.Context, eventId, targetCalendarId string) (event *Event, err error) {
	w, err := c.Writer()
	if err != nil {
		return nil, err
	}

	return w.MoveEvent(ctx, c.ID, eventId, targetCalendarId)
}

func (c Calendar) UpdateEvent(ctx context.Context, event Event) (*Event, error) {
	w, err := c.Writer()
	if err != nil {
		return nil, err
	}

	return w.UpdateEvent(ctx, event)
}
