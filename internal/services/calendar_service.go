package services

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/mennanov/fmutils"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1/calendarv1connect"
	commonv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/common/v1"
	idmv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/idm/v1"
	rosterv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/roster/v1"
	"github.com/tierklinik-dobersberg/apis/pkg/data"
	"github.com/tierklinik-dobersberg/apis/pkg/discovery/consuldiscover"
	"github.com/tierklinik-dobersberg/apis/pkg/discovery/wellknown"
	"github.com/tierklinik-dobersberg/apis/pkg/log"
	"github.com/tierklinik-dobersberg/cis-cal/internal/app"
	"github.com/tierklinik-dobersberg/cis-cal/internal/cache"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

type CalendarService struct {
	calendarv1connect.UnimplementedCalendarServiceHandler

	// User cache and various indexes.
	users       *cache.Cache[*idmv1.Profile]
	byUserId    *cache.Index[string, *idmv1.Profile]
	userByCalId *cache.Index[string, *idmv1.Profile]

	// Calendar cache and various indexes.
	calendars    *cache.Cache[repo.Calendar]
	calendarById *cache.Index[string, repo.Calendar]

	repo *app.App
}

func New(ctx context.Context, svc *app.App) *CalendarService {

	// create a new user profile cache.
	profileCache := cache.NewCache("profiles", time.Minute*5, cache.LoaderFunc[*idmv1.Profile](func(ctx context.Context) ([]*idmv1.Profile, error) {
		res, err := svc.Users.ListUsers(ctx, connect.NewRequest(&idmv1.ListUsersRequest{
			FieldMask: &fieldmaskpb.FieldMask{
				Paths: []string{"users.user.extra", "users.user.id", "users.user.username"},
			},
		}))

		if err != nil {
			return nil, err
		}

		return res.Msg.Users, nil
	}))
	profileCache.Start(ctx)

	// create a new calendar cache
	calendarCache := cache.NewCache("calendars", time.Minute*5, cache.LoaderFunc[repo.Calendar](func(ctx context.Context) ([]repo.Calendar, error) {
		googleCals, err := svc.Google.ListCalendars(ctx)
		if err != nil {
			return nil, err
		}

		icals, err := svc.ICalRepo.ListCalendars(ctx)
		if err != nil {
			return nil, err
		}

		return append(googleCals, icals...), nil
	}))

	calendarCache.Start(ctx)

	s := &CalendarService{
		repo:  svc,
		users: profileCache,

		byUserId: cache.CreateIndex(profileCache, func(p *idmv1.Profile) (string, bool) {
			return p.User.Id, true
		}),
		userByCalId: cache.CreateIndex(profileCache, func(p *idmv1.Profile) (string, bool) {
			calId := extractCalendarId(ctx, p)
			return calId, calId != ""
		}),

		calendars: calendarCache,
		calendarById: cache.CreateIndex(calendarCache, func(c repo.Calendar) (string, bool) {
			return c.ID, true
		}),
	}

	return s
}

func (svc *CalendarService) ListCalendars(ctx context.Context, req *connect.Request[calendarv1.ListCalendarsRequest]) (*connect.Response[calendarv1.ListCalendarsResponse], error) {
	res, _ := svc.calendars.Get()

	response := &calendarv1.ListCalendarsResponse{}

	for _, cal := range res {
		var userId string
		if user, ok := svc.userByCalId.Get(cal.ID); ok {
			userId = user.User.Id
		}

		protoCal := cal.ToProto()
		protoCal.UserId = userId

		response.Calendars = append(response.Calendars, protoCal)
	}

	if req.Msg.IncludeVirtualResourceCalendars {
		res, err := svc.repo.Resources.List(ctx)
		if err != nil {
			return nil, err
		}

		for _, r := range res {
			display := r.DisplayName
			if display == "" {
				display = r.Name
			}

			response.Calendars = append(response.Calendars, &calendarv1.Calendar{
				Id:                r.Name,
				Name:              display,
				Timezone:          time.Local.String(),
				Color:             r.Color,
				IsVirtualResource: true,
				Readonly:          true,
			})
		}
	}

	return connect.NewResponse(response), nil
}

func (svc *CalendarService) ListEvents(ctx context.Context, req *connect.Request[calendarv1.ListEventsRequest]) (*connect.Response[calendarv1.ListEventsResponse], error) {
	var (
		opts  []repo.SearchOption
		start time.Time
		end   time.Time
	)

	switch v := req.Msg.SearchTime.(type) {
	case *calendarv1.ListEventsRequest_Date:
		var (
			day time.Time
			err error
		)

		if strings.Contains(v.Date, "/") {
			day, err = time.ParseInLocation("2006/01/02", v.Date, time.Local)
		} else {
			day, err = time.ParseInLocation("2006-01-02", v.Date, time.Local)
		}

		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid format for date field, expected YYYY-MM-DD or YYYY/MM/DD"))
		}

		nextDay := day.Add(time.Hour * 24)

		start = day
		end = nextDay

		opts = append(opts, []repo.SearchOption{
			repo.WithEventsAfter(day),
			repo.WithEventsBefore(nextDay),
		}...)

	case *calendarv1.ListEventsRequest_TimeRange:
		if v.TimeRange.From != nil && v.TimeRange.From.IsValid() {
			opts = append(opts, repo.WithEventsAfter(v.TimeRange.From.AsTime().Local()))
			start = v.TimeRange.From.AsTime()
		}

		if v.TimeRange.To != nil && v.TimeRange.To.IsValid() {
			opts = append(opts, repo.WithEventsBefore(v.TimeRange.To.AsTime().Local()))
			end = v.TimeRange.To.AsTime()
		}
	}

	readMask := []string{"results.calendar", "results.events"}
	if req.Msg.ReadMask != nil && len(req.Msg.ReadMask.Paths) > 0 {
		readMask = req.Msg.ReadMask.Paths
	}

	var (
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

	// get a list of all calendars from cache
	allCalendars, _ := svc.calendars.Get()

	// get a list of calendar ids to fetch
	calendarIds := make(map[string]struct{})
	if req.Msg.Source == nil {
		// only load the calendar assigned to the user

		log.L(ctx).Infof("no calendar ids specified, loading user profile ...")
		user, ok := svc.byUserId.Get(req.Header().Get("X-Remote-User-ID"))
		if !ok {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to get authenticated user profile"))
		}

		if calId := extractCalendarId(ctx, user); calId != "" {
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

				profiles, _ := svc.users.Get()
				for _, profile := range profiles {
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
			for _, cal := range allCalendars {
				calendarIds[cal.ID] = struct{}{}
			}

		case *calendarv1.ListEventsRequest_AllUsers:
			for calId := range svc.userByCalId.Keys() {
				calendarIds[calId] = struct{}{}
			}

		default:
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unsupported source specification"))
		}
	}

	if len(calendarIds) == 0 {
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("no calendars to query"))
	}

	calendarIdList := maps.Keys(calendarIds)
	sort.Stable(sort.StringSlice(calendarIdList))

	freeSlots := slices.Contains(req.Msg.RequestKinds, calendarv1.CalenarEventRequestKind_CALENDAR_EVENT_REQUEST_KIND_FREE_SLOTS)
	onlyFreeSlots := !slices.Contains(req.Msg.RequestKinds, calendarv1.CalenarEventRequestKind_CALENDAR_EVENT_REQUEST_KIND_EVENTS)

	shiftsByCalendarId := make(map[string][]*rosterv1.PlannedShift)

	// get the working-staff for those days and create a lookup map for all shifts, grouped-by date, grouped by calendar id.
	if freeSlots {
		shifts, err := svc.fetchRoster(ctx, start, end)
		if err != nil {
			slog.Error("failed to fetch roster for the requested date", "error", err)
		} else {
			slog.Info("got working shifts", "number-of-days", len(shifts))

			for _, shifts := range shifts {
				for _, shift := range shifts {
					for _, user := range shift.AssignedUserIds {
						profile, ok := svc.byUserId.Get(user)
						if !ok {
							slog.Warn("failed to get user profile from cache", "user-id", user)
							continue
						}

						calendarId := extractCalendarId(ctx, profile)
						if calendarId == "" {
							// this user does not have a work-calendar assigned
							continue
						}

						shiftsByCalendarId[calendarId] = append(shiftsByCalendarId[calendarId], shift)
					}
				}
			}
		}
	}

	eventsByResource := make(map[string][]*calendarv1.CalendarEvent)
	response := &calendarv1.ListEventsResponse{}

	for _, calId := range calendarIdList {
		var (
			events []repo.Event
			err    error
		)

		cal, ok := svc.calendarById.Get(calId)
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("failed to get calendar with id %q", calId))
		}

		if mustLoadEvents || freeSlots {
			events, err = cal.ListEvents(ctx, calId, opts...)
			if err != nil {
				return nil, err
			}

			sort.Stable(repo.EventList(events))

			var slots []repo.Event
			if freeSlots {
				shifts, ok := shiftsByCalendarId[calId]
				if ok {
					for _, shift := range shifts {
						var username string
						profile, ok := svc.userByCalId.Get(calId)
						if ok {
							username = profile.User.Username
						}

						slog.Info("getting free slots for shift", "user", username, "shift-id", shift.UniqueId, "workshift-id", shift.WorkShiftId, "start", shift.From.AsTime(), "to", shift.To.AsTime(), "calendar-id", calId)

						_, free, err := calculateFreeSlots(calId, shift.From.AsTime().Local(), shift.To.AsTime().Local(), events)
						if err != nil {
							slog.Error("failed to calculate free slots", "error", err, "calendar-id", calId)
						} else {
							slots = append(slots, free...)
						}
					}
				} else {
					slog.Warn("no shifts for the given calendar id", "calendar-id", calId)
				}

				if onlyFreeSlots {
					events = slots
				} else {
					events = append(events, slots...)
				}

				sort.Stable(repo.ByStartTime(events))
			}
		}

		calendarEvents := &calendarv1.CalendarEventList{
			Events: make([]*calendarv1.CalendarEvent, len(events)),
		}

		if cal, ok := svc.calendarById.Get(calId); mustLoadCalendars && ok {
			var userId string
			if user, ok := svc.userByCalId.Get(calId); ok {
				userId = user.User.Id
			}

			protoCal := cal.ToProto()
			protoCal.UserId = userId

			calendarEvents.Calendar = protoCal
		}

		for idx, e := range events {
			protoEvent, err := e.ToProto()
			if err != nil {
				return nil, err
			}

			calendarEvents.Events[idx] = protoEvent

			for _, r := range protoEvent.Resources {
				cloned := proto.Clone(protoEvent).(*calendarv1.CalendarEvent)

				cloned.VirtualCopy = true

				eventsByResource[r] = append(eventsByResource[r], cloned)
			}
		}

		// do not add empty messages
		if calendarEvents.Calendar != nil || len(calendarEvents.Events) > 0 {
			response.Results = append(response.Results, calendarEvents)
		}
	}

	// check if we should include virtual calendars
	if slices.Contains(req.Msg.RequestKinds, calendarv1.CalenarEventRequestKind_CALENDAR_EVENT_REQUEST_KIND_VIRTUAL_RESOURCES) {
		res, err := svc.repo.Resources.List(ctx)
		if err != nil {
			return nil, err
		}

		for _, r := range res {
			events := eventsByResource[r.Name]

			display := r.DisplayName
			if display == "" {
				display = r.Name
			}

			response.Results = append(response.Results, &calendarv1.CalendarEventList{
				Calendar: &calendarv1.Calendar{
					Id:                r.Name,
					Name:              display,
					Color:             r.Color,
					Timezone:          time.Local.String(),
					IsVirtualResource: true,
				},
				Events: events,
			})
		}
	}

	// make sure we don't include any values that weren't requested
	fmutils.Filter(response, readMask)

	return connect.NewResponse(response), nil
}

func (svc *CalendarService) fetchRoster(ctx context.Context, start, end time.Time) (map[string][]*rosterv1.PlannedShift, error) {
	// fetch all rosters of the configured type for the whole time range
	// we use consuldiscover here
	disc, err := consuldiscover.NewFromEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to get consul discovery client: %w", err)
	}

	rosterClient, err := wellknown.RosterService.Create(ctx, disc)
	if err != nil {
		return nil, fmt.Errorf("failed to get roster service client: %w", err)
	}

	shiftClient, err := wellknown.WorkShiftService.Create(ctx, disc)
	if err != nil {
		return nil, fmt.Errorf("failed to get workshift service client: %w", err)
	}

	// TODO(ppacher): perform the following calles in parallel

	res, err := rosterClient.GetWorkingStaff2(ctx, connect.NewRequest(&rosterv1.GetWorkingStaffRequest2{
		Query: &rosterv1.GetWorkingStaffRequest2_TimeRange{
			TimeRange: commonv1.NewTimeRange(start, end),
		},
		RosterTypeName: svc.repo.Config.FreeSlots.RosterTypeName,
	}))

	if err != nil {
		return nil, fmt.Errorf("failed to retrieve working staff: %w", err)
	}

	// load shift definitions as well
	shiftDefRes, err := shiftClient.ListWorkShifts(ctx, connect.NewRequest(&rosterv1.ListWorkShiftsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("failed to get work shift definitions: %w", err)
	}

	// create a lookup map for the shift definitions
	lm := data.IndexSlice(shiftDefRes.Msg.WorkShifts, func(item *rosterv1.WorkShift) string {
		return item.Id
	})

	shifts := make(map[string][]*rosterv1.PlannedShift, len(res.Msg.CurrentShifts))
	for _, s := range res.Msg.CurrentShifts {
		def, ok := lm[s.WorkShiftId]
		if !ok {
			slog.Warn("failed to get workshift definition", "workshift-id", s.WorkShiftId)
			continue
		}

		// skip on-call shifts
		if data.ElemInBothSlices(def.Tags, svc.repo.Config.FreeSlots.IgnoreShiftTags) {
			continue
		}

		k := s.From.AsTime().Format("2006-01-02")
		shifts[k] = append(shifts[k], s)
	}

	return shifts, nil
}

func (svc *CalendarService) CreateEvent(ctx context.Context, req *connect.Request[calendarv1.CreateEventRequest]) (*connect.Response[calendarv1.CreateEventResponse], error) {
	m := repo.Event{
		CalendarID:  req.Msg.CalendarId,
		Summary:     req.Msg.Name,
		Description: req.Msg.Description,
		StartTime:   req.Msg.Start.AsTime(),
		Resources:   req.Msg.Resources,
	}

	cal, ok := svc.calendarById.Get(req.Msg.CalendarId)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid calendar id"))
	}

	if cal.Readonly {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("calendar is read-only"))
	}

	var duration time.Duration
	if end := req.Msg.End; end != nil {
		if err := end.CheckValid(); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid value for field end: %w", err))
		}

		et := end.AsTime()
		m.EndTime = &et

		duration = m.EndTime.Sub(m.StartTime)
	} else {
		// BUG(ppacher): this isn't persisted yet!
		m.FullDayEvent = true
	}

	if extra := req.Msg.ExtraData; extra != nil {
		var err error

		m.CustomerAnnotation, err = svc.convertExtraData(ctx, extra)
		if err != nil {
			return nil, err
		}
	}

	w, err := cal.Writer()
	if err != nil {
		return nil, err
	}

	newEvent, err := w.CreateEvent(ctx, m.CalendarID, m.Summary, m.Description, m.StartTime, duration, m.Resources, m.CustomerAnnotation)
	if err != nil {
		return nil, err
	}

	protoEvent, err := newEvent.ToProto()
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&calendarv1.CreateEventResponse{
		Event: protoEvent,
	}), nil
}

func (svc *CalendarService) convertExtraData(_ context.Context, extra *anypb.Any) (*calendarv1.CustomerAnnotation, error) {
	name := extra.TypeUrl
	if strings.Contains(name, "googleapis") {
		_, name, _ = strings.Cut(name, "/")
	}

	switch name {
	case (string(new(calendarv1.CustomerAnnotation).ProtoReflect().Descriptor().FullName())):
		msg := new(calendarv1.CustomerAnnotation)

		if err := extra.UnmarshalTo(msg); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}

		return msg, nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unsupport data for ExtraData: %s", extra.TypeUrl))
	}
}

func (svc *CalendarService) UpdateEvent(ctx context.Context, req *connect.Request[calendarv1.UpdateEventRequest]) (*connect.Response[calendarv1.UpdateEventResponse], error) {
	msg := req.Msg

	cal, ok := svc.calendarById.Get(req.Msg.CalendarId)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid calendar id"))
	}

	if cal.Readonly {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("calendar is read-only"))
	}

	evt, err := svc.repo.Google.LoadEvent(ctx, msg.CalendarId, msg.EventId, true)
	if err != nil {
		return nil, err
	}

	paths := []string{
		"name",
		"description",
		"start",
		"end",
		"extra_data",
		"resources",
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
			return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("updating event.extra_data is not yet supported"))

		case "resources":
			evt.Resources = req.Msg.Resources

		default:
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid update_mask path %q", p))
		}
	}

	w, err := cal.Writer()
	if err != nil {
		return nil, err
	}

	updatedEvent, err := w.UpdateEvent(ctx, *evt)
	if err != nil {
		return nil, err
	}

	protoEvent, err := updatedEvent.ToProto()
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

	cal, ok := svc.calendarById.Get(originCalendarID)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid source calendar id"))
	}
	if cal.Readonly {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("source calendar is read-only"))
	}

	targetCalendarID := req.Msg.GetTargetCalendarId()
	if targetCalendarID == "" {
		var err error
		targetCalendarID, err = svc.resolveUserCalendar(ctx, req.Msg.GetTargetUserId())
		if err != nil {
			return nil, err
		}
	}

	// validate the target calendar
	cal, ok = svc.calendarById.Get(targetCalendarID)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid target calendar id"))
	}
	if cal.Readonly {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("target calendar is read-only"))
	}

	w, err := cal.Writer()
	if err != nil {
		return nil, err
	}

	event, err := w.MoveEvent(ctx, originCalendarID, req.Msg.EventId, targetCalendarID)
	if err != nil {
		return nil, err
	}

	protoEvent, err := event.ToProto()
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&calendarv1.MoveEventResponse{
		Event: protoEvent,
	}), nil
}

func (svc *CalendarService) resolveUserCalendar(ctx context.Context, id string) (string, error) {
	user, ok := svc.byUserId.Get(id)

	if !ok {
		return "", fmt.Errorf("failed to get user profile for id %q", id)
	}

	if cal := extractCalendarId(ctx, user); cal != "" {
		return cal, nil
	}

	return "", fmt.Errorf("no calendar associated with user %q", id)
}

func (svc *CalendarService) DeleteEvent(ctx context.Context, req *connect.Request[calendarv1.DeleteEventRequest]) (*connect.Response[calendarv1.DeleteEventResponse], error) {
	cal, ok := svc.calendarById.Get(req.Msg.CalendarId)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid calendar id"))
	}
	if cal.Readonly {
		return nil, connect.NewError(connect.CodePermissionDenied, repo.ErrReadOnly)
	}

	w, err := cal.Writer()
	if err != nil {
		return nil, err
	}

	if err := w.DeleteEvent(ctx, req.Msg.CalendarId, req.Msg.EventId); err != nil {
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

func (svc *CalendarService) StoreResourceCalendar(ctx context.Context, req *connect.Request[calendarv1.ResourceCalendar]) (*connect.Response[calendarv1.ResourceCalendar], error) {
	m := req.Msg

	if err := svc.repo.Resources.Store(ctx, m); err != nil {
		return nil, err
	}

	return connect.NewResponse(m), nil
}

func (svc *CalendarService) ListResourceCalendars(ctx context.Context, req *connect.Request[calendarv1.ListResourceCalendarsRequest]) (*connect.Response[calendarv1.ListResourceCalendarsResponse], error) {
	pb, err := svc.repo.Resources.List(ctx)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&calendarv1.ListResourceCalendarsResponse{
		ResourceCalendars: pb,
	}), nil
}

func (svc *CalendarService) DeleteResourceCalendar(ctx context.Context, req *connect.Request[calendarv1.DeleteResourceCalendarRequest]) (*connect.Response[emptypb.Empty], error) {
	if err := svc.repo.Resources.Delete(ctx, req.Msg.Name); err != nil {
		return nil, err
	}

	return connect.NewResponse(&emptypb.Empty{}), nil
}
