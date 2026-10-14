package common

import (
	"encoding/json"
	"os"
	"sync"
)

const predictorModelCatalogEnvVar = "PREDICTOR_MODEL_CATALOG"

// PredictorModelCatalogEntry is one operator-defined model in the chart's
// predictorModels catalog.
type PredictorModelCatalogEntry struct {
	Label       string `json:"label"`
	Name        string `json:"name"`
	ModelFormat string `json:"modelFormat"`
	StorageURI  string `json:"storageURI"`
	Task        string `json:"task,omitempty"`
}

var (
	predictorCatalogOnce   sync.Once
	predictorCatalogByName map[string]PredictorModelCatalogEntry
)

func loadPredictorCatalog() {
	predictorCatalogByName = make(map[string]PredictorModelCatalogEntry)
	var entries []PredictorModelCatalogEntry
	if err := json.Unmarshal([]byte(os.Getenv(predictorModelCatalogEnvVar)), &entries); err != nil {
		return
	}
	for _, e := range entries {
		if e.Name == "" || e.ModelFormat == "" || e.StorageURI == "" {
			continue
		}
		predictorCatalogByName[e.Name] = e
	}
}

// LookupPredictorModel resolves a catalog model name to its entry.
func LookupPredictorModel(name string) (PredictorModelCatalogEntry, bool) {
	predictorCatalogOnce.Do(loadPredictorCatalog)
	entry, ok := predictorCatalogByName[name]
	return entry, ok
}

// ResetPredictorCatalogForTest clears the cached catalog so tests can repoint the env var.
func ResetPredictorCatalogForTest() {
	predictorCatalogOnce = sync.Once{}
	predictorCatalogByName = nil
}
