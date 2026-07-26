// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"github.com/ruthlesslypractical/hippocampus/internal/config"
)

// OFCTunables is the frontend-facing struct for OFC neuromodulator settings.
type OFCTunables struct {
	DADecay            float64 `json:"da_decay"`
	SHTDecay           float64 `json:"sht_decay"`
	SHTBaseline        float64 `json:"sht_baseline"`
	DAExplicitPositive float64 `json:"da_explicit_positive"`
	DAExplicitNegative float64 `json:"da_explicit_negative"`
	DAImplicitPositive float64 `json:"da_implicit_positive"`
	DAImplicitNegative float64 `json:"da_implicit_negative"`
	SHTPositive        float64 `json:"sht_positive"`
	SHTNegative        float64 `json:"sht_negative"`
}

// GetOFCEnabled returns whether the OFC module is enabled.
func (a *App) GetOFCEnabled() bool {
	return a.fullConfig.OFC.Enabled
}

// SetOFCEnabled enables or disables the OFC neuromodulator module.
func (a *App) SetOFCEnabled(enabled bool) {
	a.fullConfig.OFC.Enabled = enabled
	a.saveConfig()
}

// SetOFCModel sets the model used for OFC sentiment analysis.
func (a *App) SetOFCModel(model string) {
	a.fullConfig.OFC.Model = model
	a.saveConfig()
}

// GetOFCTunables returns the current OFC tunable values for the settings UI.
func (a *App) GetOFCTunables() OFCTunables {
	return OFCTunables{
		DADecay:            a.fullConfig.OFC.DADecay,
		SHTDecay:           a.fullConfig.OFC.SHTDecay,
		SHTBaseline:        a.fullConfig.OFC.SHTBaseline,
		DAExplicitPositive: a.fullConfig.OFC.DAExplicitPositive,
		DAExplicitNegative: a.fullConfig.OFC.DAExplicitNegative,
		DAImplicitPositive: a.fullConfig.OFC.DAImplicitPositive,
		DAImplicitNegative: a.fullConfig.OFC.DAImplicitNegative,
		SHTPositive:        a.fullConfig.OFC.SHTPositive,
		SHTNegative:        a.fullConfig.OFC.SHTNegative,
	}
}

// SaveOFCTunables saves updated OFC tunable values and persists to disk.
func (a *App) SaveOFCTunables(t OFCTunables) error {
	a.fullConfig.OFC.DADecay = t.DADecay
	a.fullConfig.OFC.SHTDecay = t.SHTDecay
	a.fullConfig.OFC.SHTBaseline = t.SHTBaseline
	a.fullConfig.OFC.DAExplicitPositive = t.DAExplicitPositive
	a.fullConfig.OFC.DAExplicitNegative = t.DAExplicitNegative
	a.fullConfig.OFC.DAImplicitPositive = t.DAImplicitPositive
	a.fullConfig.OFC.DAImplicitNegative = t.DAImplicitNegative
	a.fullConfig.OFC.SHTPositive = t.SHTPositive
	a.fullConfig.OFC.SHTNegative = t.SHTNegative
	a.saveConfig()
	return nil
}

// ResetOFCTunables resets OFC tunables to their defaults.
func (a *App) ResetOFCTunables() OFCTunables {
	defaults := config.DefaultConfig().OFC
	a.fullConfig.OFC.DADecay = defaults.DADecay
	a.fullConfig.OFC.SHTDecay = defaults.SHTDecay
	a.fullConfig.OFC.SHTBaseline = defaults.SHTBaseline
	a.fullConfig.OFC.DAExplicitPositive = defaults.DAExplicitPositive
	a.fullConfig.OFC.DAExplicitNegative = defaults.DAExplicitNegative
	a.fullConfig.OFC.DAImplicitPositive = defaults.DAImplicitPositive
	a.fullConfig.OFC.DAImplicitNegative = defaults.DAImplicitNegative
	a.fullConfig.OFC.SHTPositive = defaults.SHTPositive
	a.fullConfig.OFC.SHTNegative = defaults.SHTNegative
	a.saveConfig()
	return a.GetOFCTunables()
}
