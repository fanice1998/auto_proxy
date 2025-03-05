package main

import "context"

// CloudProvider 定義雲服務提供者的抽象接口

type CloudProvider interface {
	ListRegions(ctx context.Context) ([]string, error)
	ListZones(ctx context.Context, region string) ([]string, error)
	ListMachineTypes(ctx context.Context, zone string) ([]string, error)
	RecommendedType() string
	CreateInstance(ctx context.Context, name, zone, machineType string) (string, string, error) // 返回 instanceID 和 ip
	DeleteInstance(ctx context.Context, zone, instanceID string) error
	DeleteDisk(ctx context.Context, zone, diskID string) error
	GetInstanceInfo(ctx context.Context, zone, instanceID string) (InstanceInfo, error)
}

type InstanceInfo struct {
	IP         string
	DiskID string
}