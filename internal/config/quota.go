package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 额度周期（CFG-21，T63）。
const (
	QuotaPeriodDay   = "day"
	QuotaPeriodWeek  = "week"
	QuotaPeriodMonth = "month"
	QuotaPeriodTotal = "total" // 不重置，用完为止
)

// 额度消耗策略（CFG-21）。
const (
	QuotaDrain  = "drain"  // 剩余最少的 Key 优先
	QuotaSpread = "spread" // 剩余最多的 Key 优先
)

// QuotaConfig 是池级周期额度（T63）：每把 Key 每个周期在 Unit 上最多用 Limit。
type QuotaConfig struct {
	Unit     string      `yaml:"unit"`
	Limit    int64       `yaml:"limit"`
	Period   string      `yaml:"period"`
	Reset    *QuotaReset `yaml:"reset"`
	Strategy string      `yaml:"strategy"`
}

// QuotaReset 是周期的重置时刻：day 用 Time；week 用 Weekday + Time；month 用 Day + Time。
type QuotaReset struct {
	Time    string `yaml:"time"`    // HH:MM，默认 00:00
	Weekday string `yaml:"weekday"` // mon ~ sun
	Day     int    `yaml:"day"`     // 1 ~ 31；超过当月天数时取月末
	TZ      string `yaml:"tz"`      // ±HH:MM 或 UTC，默认 UTC（与 T66 的 cooldown_until 相同）
}

// QuotaSchedule 是解析后的重置规则，供调度器计算周期边界。
type QuotaSchedule struct {
	Period       string
	Weekday      time.Weekday
	Day          int
	Hour, Minute int
	Loc          *time.Location
}

var quotaUnit = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Schedule 解析重置规则；配置已经过 CFG-21 校验时不会出错。
func (q *QuotaConfig) Schedule() (QuotaSchedule, error) {
	s := QuotaSchedule{Period: q.Period, Loc: time.UTC}
	r := q.Reset
	if r == nil {
		r = &QuotaReset{}
	}
	switch q.Period {
	case QuotaPeriodTotal:
		if q.Reset != nil {
			return s, fmt.Errorf("reset is not allowed for period total")
		}
		return s, nil
	case QuotaPeriodDay:
		if r.Weekday != "" || r.Day != 0 {
			return s, fmt.Errorf("period day only takes reset.time and reset.tz")
		}
	case QuotaPeriodWeek:
		wd, ok := weekdays[strings.ToLower(r.Weekday)]
		if !ok {
			return s, fmt.Errorf("period week needs reset.weekday (mon ~ sun), got %q", r.Weekday)
		}
		if r.Day != 0 {
			return s, fmt.Errorf("reset.day is only for period month")
		}
		s.Weekday = wd
	case QuotaPeriodMonth:
		if r.Day < 1 || r.Day > 31 {
			return s, fmt.Errorf("period month needs reset.day in [1, 31], got %d", r.Day)
		}
		if r.Weekday != "" {
			return s, fmt.Errorf("reset.weekday is only for period week")
		}
		s.Day = r.Day
	default:
		return s, fmt.Errorf("period must be day, week, month or total, got %q", q.Period)
	}
	clock := r.Time
	if clock == "" {
		clock = "00:00"
	}
	h, m, err := ParseClock(clock)
	if err != nil {
		return s, err
	}
	loc, err := ParseTZ(r.TZ)
	if err != nil {
		return s, err
	}
	s.Hour, s.Minute, s.Loc = h, m, loc
	return s, nil
}

// validateQuota 实现 CFG-21。units 是该池各服务价目表中的计量单位。
func validateQuota(path string, q *QuotaConfig, units map[string]bool) error {
	path += ".quota"
	if !quotaUnit.MatchString(q.Unit) {
		return fieldErr(path+".unit", "must match [a-z][a-z0-9_]*, got %q", q.Unit)
	}
	if !units[q.Unit] {
		return fieldErr(path+".unit", "%q is not priced or metered by any service on this pool", q.Unit)
	}
	if q.Limit < 1 {
		return fieldErr(path+".limit", "must be >= 1, got %d", q.Limit)
	}
	if q.Strategy != QuotaDrain && q.Strategy != QuotaSpread {
		return fieldErr(path+".strategy", "must be drain or spread, got %q", q.Strategy)
	}
	if _, err := q.Schedule(); err != nil {
		return fieldErr(path, "%v", err)
	}
	return nil
}

// validateQuotas 在池与服务都校验过之后检查每个池的额度声明。
func (c *Config) validateQuotas() error {
	units := map[string]map[string]bool{}
	for _, m := range c.Models {
		u := units[m.Pool]
		if u == nil {
			u = map[string]bool{}
			units[m.Pool] = u
		}
		for unit := range m.Price { // api 用量声明的单位也必须定价（CFG-20），所以只看价目表
			u[unit] = true
		}
	}
	for i, p := range c.Pools {
		if p.Quota != nil {
			if err := validateQuota(fmt.Sprintf("pools[%d]", i), p.Quota, units[p.Name]); err != nil {
				return err
			}
		}
	}
	return nil
}
