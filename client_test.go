package aviationweather

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The AWC's answers around Archerfield, abridged from real ones.
const metars = `[
	{"icaoId": "YAMB", "name": "Amberly, QL, AU", "lat": -27.63, "lon": 152.711, "obsTime": 1794546000,
	 "temp": 27, "dewp": 19, "altim": 1015, "wdir": 350, "wspd": 8, "visib": "6+", "wxString": "VCSH",
	 "clouds": [{"cover": "FEW", "base": 3200}, {"cover": "BKN", "base": 6000}],
	 "rawOb": "METAR YAMB 130500Z 35008KT 9999 VCSH FEW032 SCT045 BKN060 27/19 Q1015"},
	{"icaoId": "YBBN", "name": "Brisbane Intl, QL, AU", "lat": -27.391, "lon": 153.13, "obsTime": 1794546000,
	 "temp": 27, "dewp": 22, "altim": 1015, "wdir": "VRB", "wspd": 3, "wgst": null, "visib": 6.21, "wxString": null,
	 "clouds": [{"cover": "SCT", "base": 2400}],
	 "rawOb": "METAR YBBN 130500Z VRB03KT 9999 SCT024CB 27/22 Q1015"}
]`

const tafs = `[
	{"icaoId": "YBBN", "name": "Brisbane Intl, QL, AU", "lat": -27.391, "lon": 153.13,
	 "issueTime": "2026-11-13T05:05:00.000Z", "validTimeFrom": 1794549600, "validTimeTo": 1794657600,
	 "rawTAF": "TAF AMD YBBN 130505Z 1306/1412 02018KT 9999 SCT025 FM131000 36012KT 9999 SCT015 PROB30 TEMPO 1306/1310 VRB25G35KT 1000 TSRA BKN008 FEW070CB",
	 "fcsts": [
		{"timeFrom": 1794549600, "timeTo": 1794564000, "fcstChange": null, "probability": null, "wdir": 20, "wspd": 18, "wgst": null, "visib": "6+", "wxString": null, "clouds": [{"cover": "SCT", "base": 2500, "type": null}]},
		{"timeFrom": 1794564000, "timeTo": 1794621600, "fcstChange": "FM", "probability": null, "wdir": 360, "wspd": 12, "visib": "6+", "clouds": [{"cover": "SCT", "base": 1500, "type": null}]},
		{"timeFrom": 1794549600, "timeTo": 1794564000, "fcstChange": "TEMPO", "probability": 30, "wdir": "VRB", "wspd": 25, "wgst": 35, "visib": 0.62, "wxString": "TSRA", "clouds": [{"cover": "BKN", "base": 800, "type": null}, {"cover": "FEW", "base": 7000, "type": "CB"}]}
	]}
]`

// fake serves the fixtures: everything for a bounding box, and the
// stations asked for by ids (the AWC's 204 when none of them report).
func fake(t *testing.T) *Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-agent/1.0", r.Header.Get("User-Agent"))
		assert.Equal(t, "json", r.URL.Query().Get("format"))

		var body string
		switch r.URL.Path {
		case "/api/data/metar":
			body = metars
		case "/api/data/taf":
			body = tafs
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if ids := r.URL.Query().Get("ids"); ids != "" {
			body = only(t, body, strings.Split(ids, ","))
		} else {
			assert.NotEmpty(t, r.URL.Query().Get("bbox"))
		}
		if body == "[]" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(nil, Config{URL: srv.URL, UserAgent: "test-agent/1.0"})
	require.NoError(t, err)
	return c
}

// only keeps the reports from the stations listed.
func only(t *testing.T, body string, ids []string) string {
	var all []map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &all))
	keep := []map[string]any{}
	for _, r := range all {
		for _, id := range ids {
			if r["icaoId"] == id {
				keep = append(keep, r)
			}
		}
	}
	out, err := json.Marshal(keep)
	require.NoError(t, err)
	return string(out)
}

func TestNewClient(t *testing.T) {
	c, err := NewClient(nil, Config{})
	require.NoError(t, err)
	assert.Equal(t, DefaultURL, c.url)
	assert.Equal(t, DefaultUserAgent, c.userAgent)
	assert.Equal(t, DefaultTimeout, c.httpClient.Timeout)

	_, err = NewClient(nil, Config{URL: "http://bad\x7f"})
	require.Error(t, err)
}

func TestGetMETARs(t *testing.T) {
	c := fake(t)

	metars, err := c.GetMETARs(context.Background(), "ybbn", " YAMB ")
	require.NoError(t, err)
	require.Len(t, metars, 2)
	for _, m := range metars {
		assert.True(t, m.Own)
		assert.Zero(t, m.DistanceKm)
	}

	m, err := c.GetMETAR(context.Background(), "YBBN")
	require.NoError(t, err)
	assert.Equal(t, "YBBN", m.ICAO)
	assert.InDelta(t, -27.391, m.Lat, 1e-9)
	assert.True(t, m.CB)

	_, err = c.GetMETAR(context.Background(), "YXXX")
	require.ErrorIs(t, err, ErrNoReport)
}

func TestGetTAF(t *testing.T) {
	c := fake(t)

	taf, err := c.GetTAF(context.Background(), "YBBN")
	require.NoError(t, err)
	assert.True(t, taf.Own)
	assert.Len(t, taf.Groups, 3)

	_, err = c.GetTAF(context.Background(), "YAMB")
	require.ErrorIs(t, err, ErrNoReport, "Amberley has no TAF in the fixtures")
}

func TestStationValidation(t *testing.T) {
	c := fake(t)

	for _, bad := range [][]string{nil, {""}, {"YB"}, {"YBBN,YSSY"}, {"YB BN"}} {
		_, err := c.GetMETARs(context.Background(), bad...)
		require.Error(t, err, "%q", bad)
	}

	_, err := c.NearestMETAR(context.Background(), "", 91, 0)
	require.ErrorContains(t, err, "invalid coordinates")
}

func TestNearestMETAR(t *testing.T) {
	c := fake(t)

	// Archerfield: Brisbane's the nearer
	m, err := c.NearestMETAR(context.Background(), "YBAF", -27.5703, 153.0081)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "YBBN", m.ICAO)
	assert.False(t, m.Own)
	assert.InDelta(t, 23, m.DistanceKm, 3)
	assert.Nil(t, m.WindDir, "VRB has no direction")
	assert.Equal(t, 3, m.WindKt)
	assert.True(t, m.CB, "CB from the raw report")
	assert.Equal(t, 10.0, m.VisibilityKm)
	require.NotNil(t, m.QNH)
	assert.Equal(t, 1015.0, *m.QNH)
	require.NotNil(t, m.TempC)
	assert.Equal(t, 27.0, *m.TempC)

	// at Amberley, its own report, however near Brisbane is
	m, err = c.NearestMETAR(context.Background(), "YAMB", -27.5703, 153.0081)
	require.NoError(t, err)
	assert.Equal(t, "YAMB", m.ICAO)
	assert.True(t, m.Own)
	assert.Equal(t, 6000, m.CeilingFt)
	assert.Equal(t, "BKN", m.Cover)
	require.NotNil(t, m.WindDir)
	assert.Equal(t, 350, *m.WindDir)
}

func TestNearestTAF(t *testing.T) {
	taf, err := fake(t).NearestTAF(context.Background(), "YBAF", -27.5703, 153.0081)
	require.NoError(t, err)
	require.NotNil(t, taf)
	assert.Equal(t, "YBBN", taf.ICAO)
	assert.Equal(t, int64(1794546300), taf.IssuedAt, "05:05Z")
	require.Len(t, taf.Groups, 3)

	assert.Equal(t, "", taf.Groups[0].Change)
	assert.Equal(t, 20, *taf.Groups[0].WindDir)

	assert.Equal(t, "FM", taf.Groups[1].Change)
	assert.Equal(t, 360, *taf.Groups[1].WindDir)
	assert.Equal(t, int64(1794564000), taf.Groups[1].From)

	storm := taf.Groups[2]
	assert.Equal(t, "TEMPO", storm.Change)
	assert.Equal(t, 30, storm.Probability)
	assert.Nil(t, storm.WindDir)
	assert.Equal(t, 35, storm.GustKt)
	assert.Equal(t, "TSRA", storm.Weather)
	assert.True(t, storm.CB)
	assert.InDelta(t, 1.0, storm.VisibilityKm, 0.01)
	assert.Equal(t, []Cloud{{Cover: "BKN", BaseFt: 800}, {Cover: "FEW", BaseFt: 7000, Type: "CB"}}, storm.Clouds)
}

func TestNothingNearby(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := NewClient(nil, Config{URL: srv.URL})
	require.NoError(t, err)

	_, err = c.NearestMETAR(context.Background(), "YXXX", 0, 0)
	require.ErrorIs(t, err, ErrNoReport)
	_, err = c.NearestTAF(context.Background(), "", 0, 0)
	require.ErrorIs(t, err, ErrNoReport)
}

func TestErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Invalid bbox"))
	}))
	defer srv.Close()

	c, err := NewClient(nil, Config{URL: srv.URL})
	require.NoError(t, err)

	_, err = c.NearestMETAR(context.Background(), "", 0, 0)
	require.ErrorContains(t, err, "status 400: Invalid bbox")
	assert.NotErrorIs(t, err, ErrNoReport)
}

func TestCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, err := NewClient(nil, Config{URL: srv.URL})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = c.GetMETAR(ctx, "YBBN")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestVisibility(t *testing.T) {
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	assert.Equal(t, 10.0, visibilityKm(raw("6+")))
	assert.InDelta(t, 0.8, visibilityKm(raw("1/2")), 0.01)
	assert.InDelta(t, 3.2, visibilityKm(raw(2)), 0.01)
	assert.Equal(t, 10.0, visibilityKm(raw(nil)))
	assert.Equal(t, 10.0, visibilityKm(nil))
}

func TestRestoreInter(t *testing.T) {
	// Brisbane's, as the AWC gives it: the INTER filed as a TEMPO
	taf := &TAF{
		Raw:    "TAF AMD YBBN 130505Z 1306/1412 02018KT 9999 SCT025 FM131000 36012KT 9999 SCT015 FM140200 02016KT 9999 SCT030 TEMPO 1312/1322 9999 BKN010 INTER 1316/1404 3000 SHRA BKN008 FEW050TCU",
		Groups: []TAFGroup{{}, {Change: "FM"}, {Change: "FM"}, {Change: "TEMPO"}, {Change: "TEMPO"}},
	}
	restoreInter(taf)
	assert.Equal(t, "TEMPO", taf.Groups[3].Change)
	assert.Equal(t, "INTER", taf.Groups[4].Change)

	// PROB30 INTER is one group; when the counts don't line up, nothing's touched
	taf = &TAF{
		Raw:    "TAF YSSY 130215Z 1303/1406 04017KT 9999 FEW025 PROB30 INTER 1306/1310 VRB25G35KT 1000 TSRA BKN008",
		Groups: []TAFGroup{{}, {Change: "TEMPO", Probability: 30}},
	}
	restoreInter(taf)
	assert.Equal(t, "INTER", taf.Groups[1].Change)

	taf = &TAF{Raw: "TAF YXXX 1303/1406 04017KT INTER 1306/1310 3000", Groups: []TAFGroup{{}, {Change: "TEMPO"}, {Change: "TEMPO"}}}
	restoreInter(taf)
	assert.Equal(t, "TEMPO", taf.Groups[1].Change)
}
