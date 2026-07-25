package seedance

import "time"

const (
	ModelName                   = "seedance-uncensored"
	ChannelName                 = "seed-dance"
	defaultDuration             = 15
	defaultResolution           = "720P"
	normalizedRequestContextKey = "seedance_normalized_request"
	submitTimeout               = 60 * time.Second
	statusTimeout               = 30 * time.Second
	contentTimeout              = 120 * time.Second
	connectTimeout              = 10 * time.Second
)

var resolutionRatios = map[string]float64{
	"480P":  0.5,
	"720P":  1.0,
	"1080P": 2.25,
}
