package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/mennanov/fmutils"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1/calendarv1connect"
	idmv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/idm/v1"
	"github.com/tierklinik-dobersberg/apis/pkg/log"
	"github.com/tierklinik-dobersberg/cis-cal/internal/app"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type CalendarService struct {
	calendarv1connect.UnimplementedCalendarServiceHandler

	repo *app.App
}

func New(svc *app.App) *CalendarService {
	return &CalendarService{repo: svc}
}

func (svc *CalendarService) ListCalendars(ctx context.Context, req *connect.Request[calendarv1.ListCalendarsRequest]) (*connect.Response[calendarv1.ListCalendarsResponse], error) {
	res, err := svc.repo.ListCalendars(ctx)
	if err != nil {
		return nil, err
	}

	response := &calendarv1.ListCalendarsResponse{}

	for _, cal := range res {
		response.Calendars = append(response.Calendars, &calendarv1.Calendar{
			Id:       cal.ID,
			Name:     cal.Name,
			Timezone: cal.Timezone,
			Color:    cal.Color,
		})
	}

	return connect.NewResponse(response), nil
}

func (svc *CalendarService) ListEvents(ctx context.Context, req *connect.Request[calendarv1.ListEventsRequest]) (*connect.Response[calendarv1.ListEventsResponse], error) {
	var opts []repo.SearchOption

	switch v := req.Msg.SearchTime.(type) {
	case *calendarv1.ListEventsRequest_Date:
		day, err := time.Parse("2006/01/02", v.Date)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}

		nextDay := day.Add(time.Hour * 24)

		opts = append(opts, []repo.SearchOption{
			repo.WithEventsAfter(day),
			repo.WithEventsBefore(nextDay),
		}...)
	case *calendarv1.ListEventsRequest_TimeRange:
		if v.TimeRange.From != nil {
			opts = append(opts, repo.WithEventsAfter(v.TimeRange.From.AsTime()))
		}

		if v.TimeRange.To != nil {
			opts = append(opts, repo.WithEventsBefore(v.TimeRange.To.AsTime()))
		}
	}

	readMask := []string{"results.calendar", "results.events"}
	if req.Msg.ReadMask != nil && len(req.Msg.ReadMask.Paths) > 0 {
		readMask = req.Msg.ReadMask.Paths
	}

	var (
		allCalendars      []repo.Calendar
		mustLoadCalendars bool
		mustLoadEvents    bool
	)

	for _, path := range readMask {
		switch {
		case strings.HasPrefix(path, "results.calendar"):
			mustLoadCalendars = true
		case strings.HasPrefix(path, "results.events"):
			mustLoadEvents = true
		case path == "results":
			mustLoadCalendars = true
			mustLoadEvents = true
		}
	}

	// get a list of calendar ids to fetch
	calendarIds := make(map[string]struct{})
	if req.Msg.Source == nil {
		// only load the calendar assigned to the user

		log.L(ctx).Infof("no calendar ids specified, loading user profile ...")
		user, err := svc.repo.Users.GetUser(ctx, connect.NewRequest(&idmv1.GetUserRequest{
			Search: &idmv1.GetUserRequest_Id{
				Id: req.Header().Get("X-Remote-User-ID"),
			},
			FieldMask: &fieldmaskpb.FieldMask{
				Paths: []string{"profile.user.extra", "profile.user.id"},
			},
		}))
		if err != nil {
			return nil, err
		}

		if calId := extractCalendarId(ctx, user.Msg.Profile); calId != "" {
			calendarIds[calId] = struct{}{}
		}
	} else {

		switch v := req.Msg.Source.(type) {
		case *calendarv1.ListEventsRequest_Sources:
			for _, id := range v.Sources.CalendarIds {
				calendarIds[id] = struct{}{}
			}

			if len(v.Sources.UserIds) > 0 {
				// build a lookup map for the users
				userSet := make(map[string]struct{})
				for _, usr := range v.Sources.UserIds {
					userSet[usr] = struct{}{}
				}

				users, err := svc.repo.Users.ListUsers(ctx, connect.NewRequest(&idmv1.ListUsersRequest{
					FieldMask: &fieldmaskpb.FieldMask{
						Paths: []string{"users.user.extra", "users.user.id"},
					},
				}))
				if err != nil {
					return nil, err
				}

				for _, profile := range users.Msg.Users {
					if _, ok := userSet[profile.User.Id]; !ok {
						continue
					}

					calId := extractCalendarId(ctx, profile)
					if calId != "" {
						calendarIds[calId] = struct{}{}
					}
				}
			}
		case *calendarv1.ListEventsRequest_AllCalendars:
			var err error
			allCalendars, err = svc.repo.ListCalendars(ctx)
			if err != nil {
				return nil, err
			}

			for _, cal := range allCalendars {
				calendarIds[cal.ID] = struct{}{}
			}

		case *calendarv1.ListEventsRequest_AllUsers:
			users, err := svc.repo.Users.ListUsers(ctx, connect.NewRequest(&idmv1.ListUsersRequest{
				FieldMask: &fieldmaskpb.FieldMask{
					Paths: []string{"users.user.extra"},
				},
			}))
			if err != nil {
				return nil, err
			}

			for _, profile := range users.Msg.Users {
				if calId := extractCalendarId(ctx, profile); calId != "" {
					calendarIds[calId] = struct{}{}
				}
			}
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unsupported source specification"))
		}
	}

	if len(calendarIds) == 0 {
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("no calendars to query"))
	}

	calendarById := make(map[string]repo.Calendar)

	// make sure we have all calendars loaded if requested
	if mustLoadCalendars && len(allCalendars) == 0 {
		var err error
		log.L(ctx).Infof("read_mask requests calendar data, loading calendars")
		allCalendars, err = svc.repo.ListCalendars(ctx)
		if err != nil {
			return nil, err
		}
	} else if mustLoadCalendars {
		log.L(ctx).Infof("read_mask requests calendar data, data already loaded")
	}

	// build a lookup map for the calendars (if we have some)
	for _, cal := range allCalendars {
		calendarById[cal.ID] = cal
	}

	response := &calendarv1.ListEventsResponse{}
	for calId := range calendarIds {
		var (
			events []repo.Event
			err    error
		)

		if mustLoadEvents {
			events, err = svc.repo.ListEvents(ctx, calId, opts...)
			if err != nil {
				return nil, err
			}
		}

		calendarEvents := &calendarv1.CalendarEventList{
			Events: make([]*calendarv1.CalendarEvent, len(events)),
		}

		if cal, ok := calendarById[calId]; mustLoadCalendars && ok {
			calendarEvents.Calendar = &calendarv1.Calendar{
				Id:       cal.ID,
				Name:     cal.Name,
				Timezone: cal.Timezone,
				Color:    cal.Color,
			}
		}

		for idx, e := range events {
			protoEvent, err := modelToProtoEvent(ctx, e)
			if err != nil {
				return nil, err
			}

			calendarEvents.Events[idx] = protoEvent
		}

		// do not add empty messages
		if calendarEvents.Calendar != nil || len(calendarEvents.Events) > 0 {
			response.Results = append(response.Results, calendarEvents)
		}
	}

	// make sure we don't include any values that weren't requested
	fmutils.Filter(response, readMask)

	return connect.NewResponse(response), nil
}

func modelToProtoEvent(_ context.Context, model repo.Event) (*calendarv1.CalendarEvent, error) {
	var endTime *timestamppb.Timestamp
	var any *anypb.Any
	var err error

	if model.EndTime != nil {
		endTime = timestamppb.New(*model.EndTime)
	}

	if model.Data != nil {
		extra := &calendarv1.CustomerAnnotation{
			CustomerSource:  model.Data.CustomerSource,
			CustomerId:      model.Data.CustomerID,
			AnimalIds:       model.Data.AnimalID,
			CreatedByUserId: model.Data.CreatedBy,
		}

		any, err = anypb.New(extra)
		if err != nil {
			return nil, err
		}
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
	}, nil

}

func (svc *CalendarService) CreateEvent(ctx context.Context, req *connect.Request[calendarv1.CreateEventRequest]) (*connect.Response[calendarv1.CreateEventResponse], error) {
	m := repo.Event{
		CalendarID:  req.Msg.CalendarId,
		Summary:     req.Msg.Name,
		Description: req.Msg.Description,
		StartTime:   req.Msg.Start.AsTime(),
	}

	var duration time.Duration
	if end := req.Msg.End; end != nil {
		et := end.AsTime()
		m.EndTime = &et

		duration = m.EndTime.Sub(m.StartTime)
	} else {
		m.FullDayEvent = true
	}

	if extra := req.Msg.ExtraData; extra != nil {
		var err error

		m.Data, err = svc.convertExtraData(ctx, extra)
		if err != nil {
			return nil, err
		}
	}

	newEvent, err := svc.repo.CreateEvent(ctx, m.CalendarID, m.Summary, m.Description, m.StartTime, duration, m.Data)
	if err != nil {
		return nil, err
	}

	protoEvent, err := modelToProtoEvent(ctx, *newEvent)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&calendarv1.CreateEventResponse{
		Event: protoEvent,
	}), nil
}

func (svc *CalendarService) convertExtraData(_ context.Context, extra *anypb.Any) (*repo.StructuredEvent, error) {
	switch extra.TypeUrl {
	case (string(new(calendarv1.CustomerAnnotation).ProtoReflect().Descriptor().FullName())):
		var msg calendarv1.CustomerAnnotation

		if err := extra.UnmarshalTo(&msg); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}

		return &repo.StructuredEvent{
			CustomerSource: msg.CustomerSource,
			CustomerID:     msg.CustomerId,
			AnimalID:       msg.AnimalIds,
			CreatedBy:      msg.CreatedByUserId,
		}, nil

	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unsupport data for ExtraData"))
	}
}

func (svc *CalendarService) UpdateEvent(ctx context.Context, req *connect.Request[calendarv1.UpdateEventRequest]) (*connect.Response[calendarv1.UpdateEventResponse], error) {
	msg := req.Msg

	evt, err := svc.repo.LoadEvent(ctx, msg.CalendarId, msg.EventId, true)
	if err != nil {
		return nil, err
	}

	paths := []string{
		"name",
		"description",
		"start",
		"end",
		"extra_data",
	}

	if um := msg.GetUpdateMask().GetPaths(); len(um) > 0 {
		paths = um
	}

	for _, p := range paths {
		switch p {
		case "name":
			if msg.Name == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name field is required"))
			}

			evt.Summary = msg.Name

		case "description":
			evt.Description = msg.Description

		case "start":
			if err := msg.Start.CheckValid(); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid value for field start: %w", err))
			}

			evt.StartTime = msg.Start.AsTime()

			if evt.StartTime.IsZero() {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid value for field start: %w", err))
			}

		case "end":
			if msg.End == nil {
				evt.EndTime = nil
			} else {
				if err := msg.End.CheckValid(); err != nil {
					return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid value for field end: %w", err))
				}

				endTime := msg.End.AsTime()

				if endTime.IsZero() {
					evt.EndTime = nil
				} else {
					evt.EndTime = &endTime
				}
			}

		case "extra_data":
			if extra := msg.ExtraData; extra != nil {
				evt.Data, err = svc.convertExtraData(ctx, msg.ExtraData)
			} else {
				evt.Data = nil
			}

		default:
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid update_mask path %q", p))
		}
	}

	updatedEvent, err := svc.repo.UpdateEvent(ctx, *evt)
	if err != nil {
		return nil, err
	}

	protoEvent, err := modelToProtoEvent(ctx, *updatedEvent)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&calendarv1.UpdateEventResponse{
		Event: protoEvent,
	}), nil
}

func (svc *CalendarService) MoveEvent(ctx context.Context, req *connect.Request[calendarv1.MoveEventRequest]) (*connect.Response[calendarv1.MoveEventResponse], error) {
	originCalendarID := req.Msg.GetSourceCalendarId()
	if originCalendarID == "" {
		var err error
		originCalendarID, err = svc.resolveUserCalendar(ctx, req.Msg.GetSourceUserId())
		if err != nil {
			return nil, err
		}
	}

	targetCalendarID := req.Msg.GetTargetCalendarId()
	if targetCalendarID == "" {
		var err error
		targetCalendarID, err = svc.resolveUserCalendar(ctx, req.Msg.GetTargetUserId())
		if err != nil {
			return nil, err
		}
	}

	event, err := svc.repo.MoveEvent(ctx, originCalendarID, req.Msg.EventId, targetCalendarID)
	if err != nil {
		return nil, err
	}

	protoEvent, err := modelToProtoEvent(ctx, *event)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&calendarv1.MoveEventResponse{
		Event: protoEvent,
	}), nil
}

func (svc *CalendarService) resolveUserCalendar(ctx context.Context, id string) (string, error) {
	user, err := svc.repo.Users.GetUser(ctx, connect.NewRequest(&idmv1.GetUserRequest{
		Search: &idmv1.GetUserRequest_Id{
			Id: id,
		},
		FieldMask: &fieldmaskpb.FieldMask{
			Paths: []string{"profile.user.extra"},
		},
	}))

	if err != nil {
		return "", err
	}

	if cal := extractCalendarId(ctx, user.Msg.GetProfile()); cal != "" {
		return cal, nil
	}

	return "", fmt.Errorf("no calendar associated with user %q", id)
}

func (svc *CalendarService) DeleteEvent(ctx context.Context, req *connect.Request[calendarv1.DeleteEventRequest]) (*connect.Response[calendarv1.DeleteEventResponse], error) {
	if err := svc.repo.DeleteEvent(ctx, req.Msg.CalendarId, req.Msg.EventId); err != nil {
		return nil, err
	}

	return connect.NewResponse(new(calendarv1.DeleteEventResponse)), nil
}

func extractCalendarId(ctx context.Context, profile *idmv1.Profile) string {
	if profile == nil || profile.User == nil {
		return ""
	}

	extrapb := profile.User.Extra
	if extrapb != nil {
		calVal := extrapb.Fields["calendarID"]
		if calVal != nil {
			switch v := calVal.Kind.(type) {
			case *structpb.Value_StringValue:
				return v.StringValue
			default:
				log.L(ctx).Errorf("invalid value for calendarId extra field: %s", calVal.Kind)
			}
		}
	}

	return ""
}
