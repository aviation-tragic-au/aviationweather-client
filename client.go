// Package aviationweather reads METARs and TAFs from the Aviation Weather
// Center's data API (https://aviationweather.gov/data/api/): free, keyless,
// and covering every reporting station worldwide. Reports come back
// decoded (wind, gusts, weather, cloud, ceiling, visibility, CB), with the
// raw text alongside.
//
// Besides lookups by station, NearestMETAR and NearestTAF serve an airport
// without reports of its own: they return the nearest station's, saying
// which station that is and how far away.
package aviationweather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	// DefaultURL is the AWC's site, used when Config.URL is empty.
	DefaultURL = "https://aviationweather.gov"

	// DefaultUserAgent identifies a client to the AWC, which asks clients
	// to name themselves. Replace it with your own through Config.
	DefaultUserAgent = "aviationweather-go (+https://github.com/aviation-tragic-au/aviationweather-client)"

	// DefaultTimeout bounds a request when Config.HTTPClient is unset.
	DefaultTimeout = 20 * time.Second

	// SearchDegrees is how far around a point NearestMETAR and NearestTAF
	// look for a station, in degrees each way: about 80 km of latitude.
	SearchDegrees = 0.75

	// maxBody caps a response; a large bounding box can be a lot of JSON.
	maxBody = 8 << 20
)

// ErrNoReport means no station had a report: none within SearchDegrees
// for the Nearest lookups, or none of the stations asked for.
var ErrNoReport = errors.New("aviationweather: no report")

// Config configures a Client. Everything is optional. The mapstructure
// tags let a host decode it straight from viper or similar.
type Config struct {
	// URL is the AWC's base. Empty means DefaultURL.
	URL string `json:"url" mapstructure:"url"`

	// UserAgent identifies the program to the AWC. Empty means
	// DefaultUserAgent.
	UserAgent string `json:"userAgent" mapstructure:"userAgent"`

	// HTTPClient sends the requests. Nil means a client with
	// DefaultTimeout.
	HTTPClient *http.Client `json:"-" mapstructure:"-"`
}

// Client reads the AWC's data API. It is safe for concurrent use.
type Client struct {
	logger     *zap.Logger
	httpClient *http.Client
	url        string
	userAgent  string
}

// NewClient creates an AWC client. A nil logger logs nothing.
func NewClient(logger *zap.Logger, cfg Config) (*Client, error) {
	if logger == nil {
		logger = zap.NewNop()
	}

	base := strings.TrimRight(cfg.URL, "/")
	if base == "" {
		base = DefaultURL
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("aviationweather: invalid URL: %w", err)
	}

	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}

	return &Client{
		logger:     logger.Named("aviationweather"),
		httpClient: httpClient,
		url:        base,
		userAgent:  userAgent,
	}, nil
}

// Station is where a report comes from.
type Station struct {
	// ICAO is the reporting station, e.g. "YBBN".
	ICAO string `json:"station"`
	// Name is the AWC's name for it, e.g. "Brisbane Intl, QL, AU".
	Name string `json:"stationName"`
	// Lat and Lon are the station's position.
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	// DistanceKm is how far it is from the airport asked about, for the
	// Nearest lookups; 0 for a lookup by station.
	DistanceKm float64 `json:"distanceKm"`
	// Own is set when it's the station asked about.
	Own bool `json:"own"`
}

// Conditions are the weather a METAR reports, or a TAF forecasts for one
// of its periods.
type Conditions struct {
	// WindDir is where the wind's from, degrees true; nil when it's
	// variable or calm (or, in a TAF group, not forecast to change).
	WindDir *int `json:"windDir,omitempty"`
	// WindKt and GustKt are in knots; GustKt is 0 when there are none.
	WindKt int `json:"windKt"`
	GustKt int `json:"gustKt,omitempty"`
	// Weather is the present (or forecast) weather group, e.g. "+TSRA".
	Weather string `json:"weather,omitempty"`
	// CB is set when the cloud includes cumulonimbus.
	CB bool `json:"cb,omitempty"`
	// CeilingFt is the lowest broken or overcast layer, 0 for none.
	CeilingFt int `json:"ceilingFt,omitempty"`
	// VisibilityKm is the visibility, 10 for "10 km or more".
	VisibilityKm float64 `json:"visibilityKm"`
	// Cover is the most cloud reported: "", FEW, SCT, BKN or OVC.
	Cover string `json:"cover,omitempty"`
	// Clouds are the layers, lowest first. NSC (no significant cloud, as
	// CAVOK gives) is a layer of its own with no base, so a forecast that
	// clears can be told from one that doesn't mention cloud.
	Clouds []Cloud `json:"clouds,omitempty"`
}

// Cloud is one layer.
type Cloud struct {
	// Cover is FEW, SCT, BKN, OVC, or NSC, SKC, CLR or NCD for none.
	Cover string `json:"cover"`
	// BaseFt is its base above the aerodrome, feet; 0 when there's none.
	BaseFt int `json:"baseFt,omitempty"`
	// Type is CB or TCU when it's one.
	Type string `json:"type,omitempty"`
}

// METAR is a station's latest observation.
type METAR struct {
	Station
	Conditions
	// ObservedAt is the observation's time, Unix seconds.
	ObservedAt int64 `json:"observedAt"`
	// TempC and DewpointC are in degrees; nil when not reported.
	TempC     *float64 `json:"tempC,omitempty"`
	DewpointC *float64 `json:"dewpointC,omitempty"`
	// QNH is the altimeter setting in hectopascals; nil when not reported.
	QNH *float64 `json:"qnh,omitempty"`
	// FlightCategory is the AWC's VFR, MVFR, IFR or LIFR, when given.
	FlightCategory string `json:"flightCategory,omitempty"`
	// Raw is the METAR as issued.
	Raw string `json:"raw"`
}

// TAF is a station's latest forecast.
type TAF struct {
	Station
	// IssuedAt, ValidFrom and ValidTo are Unix seconds.
	IssuedAt  int64 `json:"issuedAt"`
	ValidFrom int64 `json:"validFrom"`
	ValidTo   int64 `json:"validTo"`
	// Groups are the forecast's periods, in the order the AWC gives them:
	// the base forecast, then each change.
	Groups []TAFGroup `json:"groups"`
	// Raw is the TAF as issued.
	Raw string `json:"raw"`
}

// TAFGroup is one period of a TAF.
type TAFGroup struct {
	Conditions
	// From and To are Unix seconds.
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// Change is the group's indicator: "" for the base forecast, FM,
	// BECMG, TEMPO or INTER (Australia's intermittent changes).
	Change string `json:"change,omitempty"`
	// Probability is a PROB group's per cent, 0 otherwise.
	Probability int `json:"probability,omitempty"`
}

// GetMETARs returns the latest METAR of each station asked for (ICAO
// codes) that has one, in the AWC's order. ErrNoReport means none had.
func (c *Client) GetMETARs(ctx context.Context, icao ...string) ([]*METAR, error) {
	q, err := ids(icao)
	if err != nil {
		return nil, err
	}

	var raw []rawMETAR
	if err := c.get(ctx, "/api/data/metar", q, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNoReport
	}

	out := make([]*METAR, 0, len(raw))
	for _, m := range raw {
		out = append(out, toMETAR(m, station(m.ICAO, m.ICAO, m.Name, m.Lat, m.Lon, 0)))
	}
	return out, nil
}

// GetMETAR returns a station's latest METAR, or ErrNoReport.
func (c *Client) GetMETAR(ctx context.Context, icao string) (*METAR, error) {
	metars, err := c.GetMETARs(ctx, icao)
	if err != nil {
		return nil, err
	}
	return metars[0], nil
}

// GetTAFs returns the latest TAF of each station asked for (ICAO codes)
// that issues one, in the AWC's order. ErrNoReport means none had.
func (c *Client) GetTAFs(ctx context.Context, icao ...string) ([]*TAF, error) {
	q, err := ids(icao)
	if err != nil {
		return nil, err
	}

	var raw []rawTAF
	if err := c.get(ctx, "/api/data/taf", q, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNoReport
	}

	out := make([]*TAF, 0, len(raw))
	for _, t := range raw {
		out = append(out, toTAF(t, station(t.ICAO, t.ICAO, t.Name, t.Lat, t.Lon, 0)))
	}
	return out, nil
}

// GetTAF returns a station's latest TAF, or ErrNoReport.
func (c *Client) GetTAF(ctx context.Context, icao string) (*TAF, error) {
	tafs, err := c.GetTAFs(ctx, icao)
	if err != nil {
		return nil, err
	}
	return tafs[0], nil
}

// NearestMETAR returns the latest METAR of the station nearest (lat, lon),
// preferring icao itself when it reports; ErrNoReport when no station
// within SearchDegrees has one. icao may be empty, for a point that isn't
// an airport.
func (c *Client) NearestMETAR(ctx context.Context, icao string, lat, lon float64) (*METAR, error) {
	q, err := bbox(lat, lon)
	if err != nil {
		return nil, err
	}

	var raw []rawMETAR
	if err := c.get(ctx, "/api/data/metar", q, &raw); err != nil {
		return nil, err
	}

	best, km := -1, math.MaxFloat64
	for i, m := range raw {
		if d := distance(icao, lat, lon, m.ICAO, m.Lat, m.Lon); d < km {
			best, km = i, d
		}
	}
	if best < 0 {
		return nil, ErrNoReport
	}

	m := raw[best]
	return toMETAR(m, station(icao, m.ICAO, m.Name, m.Lat, m.Lon, km)), nil
}

// NearestTAF returns the latest TAF of the station nearest (lat, lon) that
// issues one, preferring icao itself; ErrNoReport when none within
// SearchDegrees does. icao may be empty.
func (c *Client) NearestTAF(ctx context.Context, icao string, lat, lon float64) (*TAF, error) {
	q, err := bbox(lat, lon)
	if err != nil {
		return nil, err
	}

	var raw []rawTAF
	if err := c.get(ctx, "/api/data/taf", q, &raw); err != nil {
		return nil, err
	}

	best, km := -1, math.MaxFloat64
	for i, t := range raw {
		if d := distance(icao, lat, lon, t.ICAO, t.Lat, t.Lon); d < km {
			best, km = i, d
		}
	}
	if best < 0 {
		return nil, ErrNoReport
	}

	t := raw[best]
	return toTAF(t, station(icao, t.ICAO, t.Name, t.Lat, t.Lon, km)), nil
}

func toMETAR(m rawMETAR, s Station) *METAR {
	out := &METAR{
		Station:    s,
		Conditions: conditions(m.WDir, m.WSpd, m.WGst, m.WxString, m.Clouds, m.Visib, m.Raw),
		ObservedAt: m.ObsTime,
		TempC:      m.Temp,
		DewpointC:  m.Dewp,
		QNH:        m.Altim,
		Raw:        m.Raw,
	}
	if m.FltCat != nil {
		out.FlightCategory = *m.FltCat
	}
	// calm reads 00000KT: no direction to speak of
	if out.WindKt == 0 {
		out.WindDir = nil
	}
	return out
}

func toTAF(t rawTAF, s Station) *TAF {
	out := &TAF{
		Station:   s,
		IssuedAt:  parseTime(t.IssueTime),
		ValidFrom: t.ValidTimeFrom,
		ValidTo:   t.ValidTimeTo,
		Raw:       t.RawTAF,
	}
	for _, f := range t.Fcsts {
		g := TAFGroup{
			Conditions: conditions(f.WDir, f.WSpd, f.WGst, f.WxString, f.Clouds, f.Visib, ""),
			From:       f.TimeFrom,
			To:         f.TimeTo,
		}
		if f.FcstChange != nil {
			g.Change = strings.ToUpper(*f.FcstChange)
		}
		if f.Probability != nil {
			g.Probability = *f.Probability
		}
		out.Groups = append(out.Groups, g)
	}
	restoreInter(out)
	return out
}

// restoreInter puts back Australia's INTER, which the AWC files as TEMPO:
// they're not the same (INTER's changes last under 30 minutes, TEMPO's 30
// to 60). The raw TAF still says which, so its change indicators are read
// in order and laid over the groups after the base forecast, when they
// line up one for one.
func restoreInter(t *TAF) {
	var indicators []string
	fields := strings.Fields(t.Raw)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case strings.HasPrefix(f, "FM") && len(f) == 8:
			indicators = append(indicators, "FM")
		case f == "BECMG" || f == "TEMPO" || f == "INTER":
			indicators = append(indicators, f)
		case strings.HasPrefix(f, "PROB"):
			// a PROB with TEMPO or INTER after it is one group
			change := ""
			if i+1 < len(fields) && (fields[i+1] == "TEMPO" || fields[i+1] == "INTER") {
				change = fields[i+1]
				i++
			}
			indicators = append(indicators, change)
		}
	}

	if len(t.Groups) == 0 || len(indicators) != len(t.Groups)-1 {
		return
	}
	for i, ind := range indicators {
		g := &t.Groups[i+1]
		if ind == "INTER" && g.Change == "TEMPO" {
			g.Change = "INTER"
		}
	}
}

// ids builds the query for stations by ICAO code.
func ids(icao []string) (url.Values, error) {
	var codes []string
	for _, code := range icao {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code == "" {
			continue
		}
		if len(code) != 4 || strings.ContainsAny(code, ", ") {
			return nil, fmt.Errorf("aviationweather: invalid ICAO code %q", code)
		}
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		return nil, errors.New("aviationweather: a station is required")
	}

	q := url.Values{}
	q.Set("ids", strings.Join(codes, ","))
	q.Set("format", "json")
	return q, nil
}

// bbox builds the query for the stations within SearchDegrees of a point.
func bbox(lat, lon float64) (url.Values, error) {
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil, fmt.Errorf("aviationweather: invalid coordinates %v,%v", lat, lon)
	}

	q := url.Values{}
	q.Set("bbox", fmt.Sprintf("%.3f,%.3f,%.3f,%.3f", lat-SearchDegrees, lon-SearchDegrees, lat+SearchDegrees, lon+SearchDegrees))
	q.Set("format", "json")
	return q, nil
}

// get fetches path into target. An empty answer (the AWC's 204, when no
// station has a report) leaves target empty.
func (c *Client) get(ctx context.Context, path string, q url.Values, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+path+"?"+q.Encode(), http.NoBody)
	if err != nil {
		return fmt.Errorf("aviationweather: unable to build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("aviationweather: request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusNoContent {
		return nil
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("aviationweather: %s returned status %d: %s", path, res.StatusCode, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(target); err != nil {
		c.logger.Debug("unable to parse response", zap.String("path", path), zap.Error(err))
		return fmt.Errorf("aviationweather: unable to parse %s response: %w", path, err)
	}
	return nil
}

func station(asked, icao, name string, lat, lon, km float64) Station {
	return Station{
		ICAO:       strings.ToUpper(icao),
		Name:       name,
		Lat:        lat,
		Lon:        lon,
		DistanceKm: math.Round(km),
		Own:        strings.EqualFold(icao, asked),
	}
}

// distance from the airport asked about, 0 for the airport itself.
func distance(asked string, lat, lon float64, icao string, sLat, sLon float64) float64 {
	if asked != "" && strings.EqualFold(icao, asked) {
		return 0
	}
	return DistanceKm(lat, lon, sLat, sLon)
}

// DistanceKm is the great-circle distance between two points.
func DistanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthKm = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)

	return 2 * earthKm * math.Asin(math.Sqrt(a))
}

func parseTime(s string) int64 {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}

// The AWC's JSON. wdir is a number or "VRB"; visib a number (statute
// miles) or a string such as "6+".

type rawMETAR struct {
	ICAO     string          `json:"icaoId"`
	Name     string          `json:"name"`
	Lat      float64         `json:"lat"`
	Lon      float64         `json:"lon"`
	ObsTime  int64           `json:"obsTime"`
	Temp     *float64        `json:"temp"`
	Dewp     *float64        `json:"dewp"`
	Altim    *float64        `json:"altim"`
	WDir     json.RawMessage `json:"wdir"`
	WSpd     *float64        `json:"wspd"`
	WGst     *float64        `json:"wgst"`
	Visib    json.RawMessage `json:"visib"`
	WxString *string         `json:"wxString"`
	FltCat   *string         `json:"fltCat"`
	Clouds   []rawCloud      `json:"clouds"`
	Raw      string          `json:"rawOb"`
}

type rawTAF struct {
	ICAO          string    `json:"icaoId"`
	Name          string    `json:"name"`
	Lat           float64   `json:"lat"`
	Lon           float64   `json:"lon"`
	IssueTime     string    `json:"issueTime"`
	ValidTimeFrom int64     `json:"validTimeFrom"`
	ValidTimeTo   int64     `json:"validTimeTo"`
	RawTAF        string    `json:"rawTAF"`
	Fcsts         []rawFcst `json:"fcsts"`
}

type rawFcst struct {
	TimeFrom    int64           `json:"timeFrom"`
	TimeTo      int64           `json:"timeTo"`
	FcstChange  *string         `json:"fcstChange"`
	Probability *int            `json:"probability"`
	WDir        json.RawMessage `json:"wdir"`
	WSpd        *float64        `json:"wspd"`
	WGst        *float64        `json:"wgst"`
	Visib       json.RawMessage `json:"visib"`
	WxString    *string         `json:"wxString"`
	Clouds      []rawCloud      `json:"clouds"`
}

type rawCloud struct {
	Cover string  `json:"cover"`
	Base  *int    `json:"base"`
	Type  *string `json:"type"`
}

var coverRank = map[string]int{"FEW": 1, "SCT": 2, "BKN": 3, "OVC": 4, "OVX": 4}

func conditions(wdir json.RawMessage, wspd, wgst *float64, wx *string, clouds []rawCloud, visib json.RawMessage, raw string) Conditions {
	var c Conditions

	// a number is a direction; "VRB" (or nothing) leaves it unset
	var dir float64
	if len(wdir) > 0 && string(wdir) != "null" {
		if err := json.Unmarshal(wdir, &dir); err == nil {
			d := int(math.Round(dir))
			c.WindDir = &d
		}
	}
	if wspd != nil {
		c.WindKt = int(math.Round(*wspd))
	}
	if wgst != nil {
		c.GustKt = int(math.Round(*wgst))
	}
	if wx != nil {
		c.Weather = strings.TrimSpace(*wx)
		// "no significant weather" is none
		if c.Weather == "NSW" {
			c.Weather = ""
		}
	}

	for _, cl := range clouds {
		layer := Cloud{Cover: cl.Cover}
		if cl.Base != nil {
			layer.BaseFt = *cl.Base
		}
		if cl.Type != nil {
			layer.Type = *cl.Type
		}
		c.Clouds = append(c.Clouds, layer)

		if (cl.Type != nil && *cl.Type == "CB") || strings.Contains(cl.Cover, "CB") {
			c.CB = true
		}
		if coverRank[cl.Cover] > coverRank[c.Cover] {
			c.Cover = cl.Cover
		}
		if coverRank[cl.Cover] >= 3 && cl.Base != nil && (c.CeilingFt == 0 || *cl.Base < c.CeilingFt) {
			c.CeilingFt = *cl.Base
		}
	}
	// the AWC's cloud groups don't always carry the type: the raw report does
	for _, field := range strings.Fields(raw) {
		if strings.HasSuffix(field, "CB") && len(field) >= 8 {
			c.CB = true
		}
	}

	c.VisibilityKm = visibilityKm(visib)
	sort.SliceStable(c.Clouds, func(i, j int) bool { return c.Clouds[i].BaseFt < c.Clouds[j].BaseFt })

	return c
}

// visibilityKm reads the AWC's visibility (statute miles, or a string such
// as "6+" or "1/2") as kilometres, capping it at 10 ("10 km or more").
func visibilityKm(raw json.RawMessage) float64 {
	const kmPerMile = 1.609344

	// none given: nothing to say it's poor
	if len(raw) == 0 || string(raw) == "null" {
		return 10
	}

	var miles float64
	if err := json.Unmarshal(raw, &miles); err != nil {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 10
		}
		s = strings.TrimSpace(s)
		// "6+" is the AWC's way of writing 9999: 10 km or more
		if strings.HasSuffix(s, "+") {
			return 10
		}
		if num, den, ok := strings.Cut(s, "/"); ok {
			n, err1 := strconv.ParseFloat(num, 64)
			d, err2 := strconv.ParseFloat(den, 64)
			if err1 != nil || err2 != nil || d == 0 {
				return 10
			}
			miles = n / d
		} else if v, err := strconv.ParseFloat(s, 64); err == nil {
			miles = v
		} else {
			return 10
		}
	}

	return math.Min(10, math.Round(miles*kmPerMile*10)/10)
}
