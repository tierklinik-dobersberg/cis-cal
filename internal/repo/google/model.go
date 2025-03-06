package google

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

func googleEventToModel(_ context.Context, calid string, item *calendar.Event) (*repo.Event, error) {
	var (
		err   error
		start time.Time
		end   *time.Time
	)

	if item == nil {
		return nil, fmt.Errorf("%w: received nil item", repo.ErrInvalidEvent)
	}

	if item.Start == nil {
		logrus.WithFields(logrus.Fields{
			"event": item,
		}).Errorf("failed to process google calendar event: event.Start == nil")

		return nil, fmt.Errorf("%w: event with ID %s does not have start time", repo.ErrInvalidEvent, item.Id)
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
	var resources []string

	if item.ExtendedProperties != nil && len(item.ExtendedProperties.Shared) > 0 {
		if value, ok := item.ExtendedProperties.Shared["tkd.calendar.v1.CustomerAnnoation"]; ok {
			ca = new(calendarv1.CustomerAnnotation)

			if err := protojson.Unmarshal([]byte(value), ca); err != nil {
				slog.Error("failed to unmarshal customer annoation", "error", err)
				ca = nil
			}
		}

		if value, ok := item.ExtendedProperties.Shared["tkd.calendar.v1.ResourceNames"]; ok {
			if err := json.Unmarshal([]byte(value), &resources); err != nil {
				slog.Error("failed to unmarshal resource-name annoation", "error", err)
			}
		}
	}

	return &repo.Event{
		ID:                 item.Id,
		Summary:            strings.TrimSpace(item.Summary),
		Description:        strings.TrimSpace(item.Description),
		Resources:          resources,
		StartTime:          start,
		EndTime:            end,
		FullDayEvent:       item.Start.DateTime == "" && item.Start.Date != "",
		CalendarID:         calid,
		CreateTime:         createTime,
		CustomerAnnotation: ca,
	}, nil
}
