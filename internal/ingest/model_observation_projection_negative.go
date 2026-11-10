//go:build projection_negative_fold

package ingest

func projectModelObservation(observation entryModelObservation) ObservedModelID {
	if !observation.present {
		return ""
	}
	return ObservedModelID(observation.value)
}

func assignProjectedModelObservation(*Turn, ObservedModelID) {}
