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

func GetEventsCommand(root *cli.Root) *cobra.Command {
	var (
		calendarIds []string
		userIds     []string
		all         string
		date        string
		from        string
		to          string
		readMask    []string
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
						logrus.Fatalf("invalid value for --from: %s", err)
					}

					search.TimeRange.From = timestamppb.New(fromTime)
				}

				if to != "" {
					toTime, err := time.Parse(time.RFC3339, to)
					if err != nil {
						logrus.Fatalf("invalid value for --to: %s", err)
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
	}

	return cmd
}
