package cmds

import (
	"github.com/bufbuild/connect-go"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/pkg/cli"
)

func GetResourceCommand(root *cli.Root) *cobra.Command {
	cmd := &cobra.Command{
		Use: "resources",
		Run: func(cmd *cobra.Command, args []string) {
			list, err := root.Calendar().ListResourceCalendars(root.Context(), connect.NewRequest(&calendarv1.ListResourceCalendarsRequest{}))
			if err != nil {
				logrus.Fatal(err)
			}

			root.Print(list.Msg)
		},
	}

	cmd.AddCommand(
		CreateResourceCommand(root),
		DeleteResourceCommand(root),
	)

	return cmd
}

func CreateResourceCommand(root *cli.Root) *cobra.Command {
	var (
		displayName      string
		maxConcurrentUse int
		color            string
	)

	cmd := &cobra.Command{
		Use:     "create name [flags]",
		Aliases: []string{"save", "store", "update"},
		Args:    cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cli := root.Calendar()

			req := &calendarv1.ResourceCalendar{
				Name:             args[0],
				DisplayName:      displayName,
				Color:            color,
				MaxConcurrentUse: uint32(maxConcurrentUse),
			}

			res, err := cli.StoreResourceCalendar(root.Context(), connect.NewRequest(req))
			if err != nil {
				logrus.Fatal(err)
			}

			root.Print(res.Msg)
		},
	}

	f := cmd.Flags()
	{
		f.StringVar(&displayName, "display-name", "", "An optional display name for the resource")
		f.StringVar(&color, "color", "", "An optional color for the resource")
		f.IntVar(&maxConcurrentUse, "max-use", 0, "Limit of how many events can use this resource that the same time")
	}

	return cmd
}

func DeleteResourceCommand(root *cli.Root) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "delete name",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			res, err := root.Calendar().DeleteResourceCalendar(
				root.Context(),
				connect.NewRequest(
					&calendarv1.DeleteResourceCalendarRequest{
						Name: args[0],
					},
				),
			)

			if err != nil {
				logrus.Fatal(err)
			}

			root.Print(res.Msg)
		},
	}

	return cmd
}
