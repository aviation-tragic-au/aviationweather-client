# aviationweather-client

[![Lint](https://github.com/aviation-tragic-au/aviationweather-client/actions/workflows/lint.yml/badge.svg)](https://github.com/aviation-tragic-au/aviationweather-client/actions/workflows/lint.yml) [![Test](https://github.com/aviation-tragic-au/aviationweather-client/actions/workflows/test.yml/badge.svg)](https://github.com/aviation-tragic-au/aviationweather-client/actions/workflows/test.yml)

A Go client for METARs and TAFs from the Aviation Weather Center's
[data API](https://aviationweather.gov/data/api/): free, keyless, and
covering every reporting station worldwide.

Reports come back decoded (wind, gusts, present weather, cloud layers,
ceiling, visibility in kilometres, cumulonimbus), with the raw text
alongside. A few of the AWC's quirks are smoothed over:

- a variable (`VRB`) or calm wind has no direction
- visibility such as `6+` (10 km or more) or `1/2` (statute miles) is read
  as kilometres
- cumulonimbus is picked up from the raw report when the cloud groups
  leave it out
- Australia's `INTER` change groups, which the AWC files as `TEMPO`, are
  restored from the raw TAF

For an airport without reports of its own, `NearestMETAR` and `NearestTAF`
return the nearest station's, saying which station that is and how far
away.

## Install

```sh
go get github.com/aviation-tragic-au/aviationweather-client
```

## Usage

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "log"

    aviationweather "github.com/aviation-tragic-au/aviationweather-client"
)

func main() {
    client, err := aviationweather.NewClient(nil, aviationweather.Config{
        UserAgent: "my-program/1.0 (+https://example.com)",
    })
    if err != nil {
        log.Fatal(err)
    }

    ctx := context.Background()

    metar, err := client.GetMETAR(ctx, "YBBN")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("%s: %d kt, %.0f km, %s\n", metar.ICAO, metar.WindKt, metar.VisibilityKm, metar.FlightCategory)

    // Archerfield (YBAF) issues neither, so this is Brisbane's, 23 km away
    taf, err := client.NearestTAF(ctx, "YBAF", -27.5703, 153.0081)
    switch {
    case errors.Is(err, aviationweather.ErrNoReport):
        fmt.Println("no TAF within reach")
    case err != nil:
        log.Fatal(err)
    default:
        fmt.Printf("TAF from %s, %.0f km away: %d groups\n", taf.ICAO, taf.DistanceKm, len(taf.Groups))
    }
}
```

### Methods

Every method takes a `context.Context` first.

| Method | Returns |
|---|---|
| `GetMETAR(ctx, icao)` / `GetMETARs(ctx, icao...)` | The latest METAR of each station |
| `GetTAF(ctx, icao)` / `GetTAFs(ctx, icao...)` | The latest TAF of each station |
| `NearestMETAR(ctx, icao, lat, lon)` | The nearest station's METAR, preferring `icao` itself |
| `NearestTAF(ctx, icao, lat, lon)` | The nearest station's TAF, preferring `icao` itself |
| `DistanceKm(lat1, lon1, lat2, lon2)` | Great-circle distance |

The nearest lookups search `SearchDegrees` (0.75°, about 80 km of
latitude) each way. No report, there or for the stations asked for, is
`ErrNoReport`.

### Configuration

| Field        | Description |
|--------------|-------------|
| `UserAgent`  | Identifies your program; the AWC asks clients to name themselves (default `DefaultUserAgent`) |
| `URL`        | Base URL (default `https://aviationweather.gov`) |
| `HTTPClient` | Your own `*http.Client` (the default times out after 20 seconds) |

`Config` carries `json` and `mapstructure` tags, so it can be decoded
straight from a config file or Viper.

## License

Copyright 2026 Dean Stalker

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
