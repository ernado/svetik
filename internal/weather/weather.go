package weather

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/go-faster/errors"
)

const defaultBase = "http://api.weatherstack.com"

// HTTPClient is the interface satisfied by *http.Client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is a Weatherstack API client.
type Client struct {
	http HTTPClient
	key  string
	base string
}

// Options configures Client construction.
type Options struct {
	// HTTP overrides the HTTP client. Defaults to http.DefaultClient.
	HTTP HTTPClient
}

func (o *Options) setDefaults() {
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
}

// New returns a Client authenticated with key.
func New(key string, options Options) *Client {
	options.setDefaults()
	return &Client{
		http: options.HTTP,
		key:  key,
		base: defaultBase,
	}
}

// RequestInfo mirrors the "request" block of the Weatherstack response.
type RequestInfo struct {
	Type     string `json:"type"`
	Query    string `json:"query"`
	Language string `json:"language"`
	Unit     string `json:"unit"`
}

// Location mirrors the "location" block of the Weatherstack response.
type Location struct {
	Name           string `json:"name"`
	Country        string `json:"country"`
	Region         string `json:"region"`
	Lat            string `json:"lat"`
	Lon            string `json:"lon"`
	TimezoneID     string `json:"timezone_id"`
	Localtime      string `json:"localtime"`
	LocaltimeEpoch int64  `json:"localtime_epoch"`
	UTCOffset      string `json:"utc_offset"`
}

// Astro mirrors the "astro" sub-object inside "current".
type Astro struct {
	Sunrise          string `json:"sunrise"`
	Sunset           string `json:"sunset"`
	Moonrise         string `json:"moonrise"`
	Moonset          string `json:"moonset"`
	MoonPhase        string `json:"moon_phase"`
	MoonIllumination int    `json:"moon_illumination"`
}

// AirQuality mirrors the "air_quality" sub-object inside "current".
type AirQuality struct {
	CO           string `json:"co"`
	NO2          string `json:"no2"`
	O3           string `json:"o3"`
	SO2          string `json:"so2"`
	PM25         string `json:"pm2_5"`
	PM10         string `json:"pm10"`
	USEPAIndex   string `json:"us-epa-index"`
	GBDefraIndex string `json:"gb-defra-index"`
}

// Current mirrors the "current" block of the Weatherstack response.
type Current struct {
	ObservationTime     string     `json:"observation_time"`
	Temperature         int        `json:"temperature"`
	WeatherCode         int        `json:"weather_code"`
	WeatherIcons        []string   `json:"weather_icons"`
	WeatherDescriptions []string   `json:"weather_descriptions"`
	Astro               Astro      `json:"astro"`
	AirQuality          AirQuality `json:"air_quality"`
	WindSpeed           int        `json:"wind_speed"`
	WindDegree          int        `json:"wind_degree"`
	WindDir             string     `json:"wind_dir"`
	Pressure            int        `json:"pressure"`
	Precip              float64    `json:"precip"`
	Humidity            int        `json:"humidity"`
	CloudCover          int        `json:"cloudcover"`
	FeelsLike           int        `json:"feelslike"`
	UVIndex             int        `json:"uv_index"`
	Visibility          int        `json:"visibility"`
	IsDay               string     `json:"is_day"`
}

// Response is the top-level Weatherstack API response.
type Response struct {
	Request  RequestInfo `json:"request"`
	Location Location    `json:"location"`
	Current  Current     `json:"current"`
}

// apiError is returned by Weatherstack when the request fails.
type apiError struct {
	Success bool `json:"success"`
	Error   struct {
		Code int    `json:"code"`
		Type string `json:"type"`
		Info string `json:"info"`
	} `json:"error"`
}

// do executes a GET to path with q, injects the access_key, and decodes JSON into dst.
func (c *Client) do(ctx context.Context, path string, q url.Values, dst any) error {
	u, err := url.Parse(c.base + path)
	if err != nil {
		return errors.Wrap(err, "parse url")
	}

	q.Set("access_key", c.key)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.Wrap(err, "build request")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return errors.Wrap(err, "do request")
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("invalid API key")
	}

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("unexpected status %d", resp.StatusCode)
	}

	// Weatherstack signals errors with HTTP 200 + success:false.
	// Decode into a raw message first so we can check both outcomes.
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return errors.Wrap(err, "decode body")
	}

	var apiErr apiError
	if err := json.Unmarshal(raw, &apiErr); err == nil && !apiErr.Success && apiErr.Error.Code != 0 {
		return errors.Errorf("weatherstack error %d (%s): %s",
			apiErr.Error.Code, apiErr.Error.Type, apiErr.Error.Info)
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return errors.Wrap(err, "decode response")
	}

	return nil
}

// GetCurrentByName returns current weather for the given location query.
// The query can be a city name, city+country (e.g. "Moscow,RU"), IP address,
// or lat/lon pair supported by the Weatherstack API.
func (c *Client) GetCurrentByName(ctx context.Context, name, countryCode string) (*Response, error) {
	query := name
	if countryCode != "" {
		query = name + "," + countryCode
	}

	q := url.Values{}
	q.Set("query", query)
	q.Set("units", "m") // metric

	var result Response
	if err := c.do(ctx, "/current", q, &result); err != nil {
		return nil, err
	}

	return &result, nil
}
