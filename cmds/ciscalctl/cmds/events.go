package cmds

import (
	"context"
	"fmt"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/pkg/cli"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func GetMoveEventCommand(root *cli.Root) *cobra.Command {
	var (
		sourceUser bool
		targetUser bool
	)

	cmd := &cobra.Command{
		Use:  "move [originCalendarID] [eventID] [targetCalendarID]",
		Args: cobra.ExactArgs(3),
		Run: func(cmd *cobra.Command, args []string) {
			cli := root.Calendar()

			req := &calendarv1.MoveEventRequest{
				EventId: args[1],
			}

			if sourceUser {
				req.Source = &calendarv1.MoveEventRequest_SourceUserId{
					SourceUserId: args[0],
				}
			} else {
				req.Source = &calendarv1.MoveEventRequest_SourceCalendarId{
					SourceCalendarId: args[0],
				}
			}

			if targetUser {
				req.Target = &calendarv1.MoveEventRequest_TargetUserId{
					TargetUserId: args[2],
				}
			} else {
				req.Target = &calendarv1.MoveEventRequest_TargetCalendarId{
					TargetCalendarId: args[2],
				}
			}

			res, err := cli.MoveEvent(root.Context(), connect.NewRequest(req))
			if err != nil {
				logrus.Fatalf("failed to move event: %s", err)
			}

			root.Print(res.Msg)
		},
	}

	f := cmd.Flags()
	{
		f.BoolVar(&sourceUser, "source-user", false, "Interpret [originCalendarID] as a user id")
		f.BoolVar(&targetUser, "target-user", false, "Interpret [targetCalendarID] as a user id")
	}

	return cmd
}

func GetUpdateEventCommand(root *cli.Root) *cobra.Command {
	var (
		newStartTime string
		newEndTime   string
	)
	req := &calendarv1.UpdateEventRequest{
		UpdateMask: &fieldmaskpb.FieldMask{},
	}

	cmd := &cobra.Command{
		Use:  "update [calendarID] [eventID]",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			mapping := [][2]string{
				{"summary", "name"},
				{"description", "description"},
				{"from", "start"},
				{"to", "end"},
			}

			req.CalendarId = args[0]
			req.EventId = args[1]

			for _, m := range mapping {
				if !cmd.Flag(m[0]).Changed {
					continue
				}

				req.UpdateMask.Paths = append(req.UpdateMask.Paths, m[1])

				switch m[0] {
				case "from":
					fromTime, err := time.Parse(time.RFC3339, newStartTime)
					if err != nil {
						logrus.Fatalf("invalid value for --from, expected format %q: %s", time.RFC3339, err)
					}

					req.Start = timestamppb.New(fromTime)

				case "to":
					endTime, err := time.Parse(time.RFC3339, newEndTime)
					if err != nil {
						logrus.Fatalf("invalid value for --to, expected format %q: %s", time.RFC3339, err)
					}

					req.End = timestamppb.New(endTime)
				}
			}

			if len(req.UpdateMask.Paths) == 0 {
				logrus.Fatalf("no changes specified")
			}

			res, err := root.Calendar().UpdateEvent(root.Context(), connect.NewRequest(req))
			if err != nil {
				logrus.Fatalf("failed to update event: %s", err)
			}

			root.Print(res.Msg)
		},
	}

	f := cmd.Flags()
	{
		f.StringVar(&req.Name, "summary", "", "The new event summary")
		f.StringVar(&req.Description, "description", "", "The new event description")
		f.StringVar(&newStartTime, "from", "", "The new start time for the event")
		f.StringVar(&newEndTime, "to", "", "The new end time for the event")
	}

	return cmd
}

func GetEventsCommand(root *cli.Root) *cobra.Command {
	var (
		calendarIds   []string
		userIds       []string
		all           string
		date          string
		from          string
		to            string
		readMask      []string
		freeSlots     bool
		onlyFreeSlots bool
	)

	cmd := &cobra.Command{
		Use: "events",
		Run: func(cmd *cobra.Command, args []string) {
			cli := root.Calendar()

			req := &calendarv1.ListEventsRequest{}

			if date != "" {
				_, err := time.Parse("2006/01/02", date)
				if err != nil {
					logrus.Fatalf("invalid value for --date: %s", err)
				}

				req.SearchTime = &calendarv1.ListEventsRequest_Date{
					Date: date,
				}
			} else if from != "" || to != "" {
				search := &calendarv1.ListEventsRequest_TimeRange{}

				if from != "" {
					fromTime, err := time.Parse(time.RFC3339, from)
					if err != nil {
						logrus.Fatalf("invalid value for --from: %s, expected format %q", err, time.RFC3339)
					}

					search.TimeRange.From = timestamppb.New(fromTime)
				}

				if to != "" {
					toTime, err := time.Parse(time.RFC3339, to)
					if err != nil {
						logrus.Fatalf("invalid value for --to: %s, expected format %q", err, time.RFC3339)
					}

					search.TimeRange.To = timestamppb.New(toTime)
				}
			}

			switch all {
			case "users":
				req.Source = &calendarv1.ListEventsRequest_AllUsers{
					AllUsers: true,
				}
			case "calendars":
				req.Source = &calendarv1.ListEventsRequest_AllCalendars{
					AllCalendars: true,
				}
			case "":
				if len(calendarIds) > 0 || len(userIds) > 0 {
					req.Source = &calendarv1.ListEventsRequest_Sources{
						Sources: &calendarv1.EventSource{
							CalendarIds: calendarIds,
							UserIds:     userIds,
						},
					}
				}
			default:
				logrus.Fatalf("invalid value for --all")
			}

			if len(readMask) > 0 {
				req.ReadMask = &fieldmaskpb.FieldMask{
					Paths: []string{},
				}

				for _, field := range readMask {
					req.ReadMask.Paths = append(req.ReadMask.Paths, fmt.Sprintf("results.%s", field))
				}
			}

			if freeSlots {
				req.RequestKinds = []calendarv1.CalenarEventRequestKind{
					calendarv1.CalenarEventRequestKind_CALENDAR_EVENT_REQUEST_KIND_EVENTS,
					calendarv1.CalenarEventRequestKind_CALENDAR_EVENT_REQUEST_KIND_FREE_SLOTS,
				}
			}

			if onlyFreeSlots {
				req.RequestKinds = []calendarv1.CalenarEventRequestKind{
					calendarv1.CalenarEventRequestKind_CALENDAR_EVENT_REQUEST_KIND_FREE_SLOTS,
				}
			}

			events, err := cli.ListEvents(context.Background(), connect.NewRequest(req))
			if err != nil {
				logrus.Fatalf("failed to get event list: %s", err)
			}

			root.Print(events.Msg)
		},
	}

	f := cmd.Flags()
	{
		f.StringSliceVar(&calendarIds, "calendar", nil, "A list of calendar IDs to query")
		f.StringSliceVar(&userIds, "user-ids", nil, "A list of user IDs to query")
		f.StringVar(&all, "all", "", "Either 'users' or 'calendars'")
		f.StringVar(&date, "date", "", "The date to query events for in format YYYY/MM/DD")
		f.StringVar(&from, "from", "", "")
		f.StringVar(&to, "to", "", "")
		f.StringSliceVar(&readMask, "fields", nil, "A list of fields to query.")
		f.BoolVar(&freeSlots, "include-free", false, "Include free slots")
		f.BoolVar(&onlyFreeSlots, "only-free", false, "Include free slots")
	}

	cmd.MarkFlagsMutuallyExclusive("include-free", "only-free")

	cmd.AddCommand(
		GetMoveEventCommand(root),
		GetUpdateEventCommand(root),
	)

	return cmd
}
