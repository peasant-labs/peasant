//go:build !projection_negative_fold

package transcript

import "github.com/peasant-labs/peasant/internal/ingest"

func projectModelObservation(observation entryModelObservation) ingest.ObservedModelID {
	if !observation.present {
		return ""
	}
	return ingest.ObservedModelID(observation.value)
}

func assignProjectedModelObservation(turn *ingest.Turn, observation ingest.ObservedModelID) {
	turn.ObservedModel = observation
}
