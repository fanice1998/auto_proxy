package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type ProxyRecord struct {
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Region     string `json:"region"`
	Zone       string `json:"zone"`
	InstanceID string `json:"instance_id"`
	IP         string `json:"ip"`
	Type       string `json:"type"`
	Location   string `json:"location"`
}

type RecordManager struct {
	filePath string
}

func NewRecordManager(filePath string) *RecordManager {
	return &RecordManager{filePath: filePath}
}

func (r *RecordManager) Load() ([]ProxyRecord, error) {
	data, err := os.ReadFile(r.filePath)
	if os.IsNotExist(err) {
		return []ProxyRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read records: %w", err)
	}
	var records []ProxyRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("failed to unmarshal records: %w", err)
	}
	return records, nil
}

func (r *RecordManager) Save(records []ProxyRecord) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal records: %w", err)
	}
	if err := os.WriteFile(r.filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write records: %w", err)
	}
	return nil
}
