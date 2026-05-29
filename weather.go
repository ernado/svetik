package lilith

import "context"

// WeatherReport is a domain summary of current weather conditions.
type WeatherReport struct {
	LocationName string
	Country      string
	Description  string
	Temperature  int
	FeelsLike    int
	Humidity     int
	WindSpeed    int
	WindDir      string
}

// WeatherProvider returns current weather, used by the AI layer as a tool.
type WeatherProvider interface {
	Current(ctx context.Context, city, countryCode string) (*WeatherReport, error)
}
