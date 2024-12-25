package services

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bufbuild/connect-go"
	calendarv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/calendar/v1/calendarv1connect"
	commonv1 "github.com/tierklinik-dobersberg/apis/gen/go/tkd/common/v1"
	"github.com/tierklinik-dobersberg/apis/pkg/log"
)

type HolidayService struct {
	calendarv1connect.UnimplementedHolidayServiceHandler

	country string
	getter  HolidayGetter
}

func NewHolidayService(country string) *HolidayService {
	getter := NewHolidayCache()

	return &HolidayService{
		country: country,
		getter:  getter,
	}
}

func holidayToProto(ctx context.Context, p PublicHoliday) *calendarv1.PublicHoliday {
	var protoType calendarv1.HolidayType

	if slices.Contains(p.Types, "Public") {
		protoType = calendarv1.HolidayType_PUBLIC
	} else {
		for _, pType := range p.Types {
			switch pType {
			case "Public":
				protoType = calendarv1.HolidayType_PUBLIC
			case "Bank":
				protoType = calendarv1.HolidayType_BANK
			case "School":
				protoType = calendarv1.HolidayType_SCHOOL
			case "Authorities":
				protoType = calendarv1.HolidayType_AUTHORITIES
			case "Optional":
				protoType = calendarv1.HolidayType_OPTIONAL
			case "Observance":
				protoType = calendarv1.HolidayType_OBSERVANCE
			default:
				log.L(ctx).Errorf("unsupported public holiday type %q", pType)

				protoType = calendarv1.HolidayType_HOLIDAY_TYPE_UNSPECIFIED

				continue
			}

			break
		}
	}

	return &calendarv1.PublicHoliday{
		Date:        p.Date,
		LocalName:   p.LocalName,
		Name:        p.Name,
		CountryCode: p.CountryCode,
		Fixed:       p.Fixed,
		Global:      p.Global,
		Type:        protoType,
	}
}

func (svc *HolidayService) GetHoliday(ctx context.Context, req *connect.Request[calendarv1.GetHolidayRequest]) (*connect.Response[calendarv1.GetHolidayResponse], error) {
	holidays, err := svc.getter.Get(ctx, svc.country, int(req.Msg.GetYear()))
	if err != nil {
		return nil, err
	}

	prefix := fmt.Sprintf("%d-", req.Msg.GetYear())
	if req.Msg.Month > 0 {
		prefix += fmt.Sprintf("%02d-", req.Msg.GetMonth())
	}

	result := make([]*calendarv1.PublicHoliday, 0, len(holidays))
	for _, p := range holidays {
		if !strings.HasPrefix(p.Date, prefix) {
			continue
		}

		result = append(result, holidayToProto(ctx, p))
	}

	return connect.NewResponse(&calendarv1.GetHolidayResponse{
		Holidays: result,
	}), nil
}

func (svc *HolidayService) IsHoliday(ctx context.Context, req *connect.Request[calendarv1.IsHolidayRequest]) (*connect.Response[calendarv1.IsHolidayResponse], error) {
	date := req.Msg.Date

	if date == nil {
		date = commonv1.FromTime(time.Now().Local())
	}

	t := date.AsTime()

	isHoliday, holiday, err := svc.getter.IsHoliday(ctx, svc.country, t)
	if err != nil {
		return nil, err
	}

	res := &calendarv1.IsHolidayResponse{
		IsHoliday:   isHoliday,
		QueriedDate: date,
	}

	if isHoliday {
		res.Holiday = holidayToProto(ctx, *holiday)
	}

	return connect.NewResponse(res), nil
}

func (svc *HolidayService) NumberOfWorkDays(ctx context.Context, req *connect.Request[calendarv1.NumberOfWorkDaysRequest]) (*connect.Response[calendarv1.NumberOfWorkDaysResponse], error) {
	from := req.Msg.From.AsTime()
	to := req.Msg.To.AsTime()

	response := &calendarv1.NumberOfWorkDaysResponse{}

	country := req.Msg.Country
	if country == "" {
		country = svc.country
	}

L:
	for iter := from; iter.Before(to) || iter.Equal(to); iter = iter.AddDate(0, 0, 1) {
		switch iter.Weekday() {
		case time.Saturday, time.Sunday:
			response.NumberOfWeekendDays++
			continue
		default:
			isHoliday, _, err := svc.getter.IsHoliday(ctx, country, iter)
			if err != nil {
				break L
			}

			if isHoliday {
				response.NumberOfHolidays++
			} else {
				response.NumberOfWorkDays++
			}
		}
	}

	return connect.NewResponse(response), nil
}
