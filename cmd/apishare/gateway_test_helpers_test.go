package main

import "testing"

func gatewayTestDB(t *testing.T) {
	t.Helper()
	modelsTestDB(t)
	if err := loadSettingsCache(); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapAdmin(); err != nil {
		t.Fatal(err)
	}
	resetModelLimitsForTest(t)
}

func resetModelLimitsForTest(t *testing.T) {
	t.Helper()
	probedModelLimits.Lock()
	oldProbed := probedModelLimits.data
	probedModelLimits.data = map[string]ModelTokenLimits{}
	probedModelLimits.Unlock()
	learnedModelLimits.Lock()
	oldLearned := learnedModelLimits.data
	learnedModelLimits.data = map[string]ModelTokenLimits{}
	learnedModelLimits.Unlock()
	t.Cleanup(func() {
		probedModelLimits.Lock()
		probedModelLimits.data = oldProbed
		probedModelLimits.Unlock()
		learnedModelLimits.Lock()
		learnedModelLimits.data = oldLearned
		learnedModelLimits.Unlock()
	})
}
