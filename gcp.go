package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

var gcp_locations = map[string]string{
    "africa-south1": "約翰尼斯堡",
    "asia-east1": "台灣",
    "asia-east2": "香港",
    "asia-northeast1": "東京",
    "asia-northeast2": "大阪",
    "asia-northeast3": "首爾",
    "asia-south1": "孟買",
    "asia-south2": "德里",
    "asia-southeast1": "新加坡",
    "asia-southeast2": "雅加達",
    "australia-southeast1": "雪梨",
    "australia-southeast2": "墨爾本",
    "europe-central2": "華沙",
    "europe-north1": "芬蘭",
    "europe-north2": "斯德哥爾摩",
    "europe-southwest1": "馬德里",
    "europe-west1": "比利時",
    "europe-west10": "柏林",
    "europe-west12": "杜林",
    "europe-west2": "倫敦",
    "europe-west3": "法蘭克福",
    "europe-west4": "荷蘭",
    "europe-west6": "蘇黎世",
    "europe-west8": "米蘭",
    "europe-west9": "巴黎",
    "me-central1": "杜哈",
    "me-central2": "達曼",
    "me-west1": "特拉維夫",
    "northamerica-northeast1": "蒙特婁",
    "northamerica-northeast2": "多倫多",
    "northamerica-south1": "墨西哥",
    "southamerica-east1": "聖保羅",
    "southamerica-west1": "聖地牙哥",
    "us-central1": "愛荷華州",
    "us-east1": "南卡羅來納州",
    "us-east4": "北維吉尼亞州",
    "us-east5": "哥倫布",
    "us-south1": "達拉斯",
    "us-west1": "奧勒岡州",
    "us-west2": "洛杉磯",
    "us-west3": "鹽湖城",
    "us-west4": "拉斯維加斯",
}

type GCPProvider struct {
	service *compute.Service
	project string
}

func NewGCPProvider(project string, credsPath string) (*GCPProvider, error) {
	ctx := context.Background()
	svc, err := compute.NewService(ctx, option.WithCredentialsFile(credsPath))
	if err != nil {
		return nil, err
	}
	return &GCPProvider{service: svc, project: project}, nil
}

func (g *GCPProvider) ListRegions(ctx context.Context) ([]string, error) {
	req := g.service.Regions.List(g.project)
	var regions []string
	err := req.Pages(ctx, func(page *compute.RegionList) error {
		for _, region := range page.Items {
			regions = append(regions, region.Name)
		}
		return nil
	})
	return regions, err
}

func (g *GCPProvider) ListZones(ctx context.Context, region string) ([]string, error) {
	req := g.service.Zones.List(g.project)
	var zones []string
	err := req.Pages(ctx, func(page *compute.ZoneList) error {
		for _, zone := range page.Items {
			if strings.HasPrefix(zone.Name, region) {
				zones = append(zones, zone.Name)
			}
		}
		return nil
	})
	return zones, err
}

func (g *GCPProvider) ListMachineTypes(ctx context.Context, zone string) ([]string, error) {
	req := g.service.MachineTypes.List(g.project, zone)
	var types []string
	err := req.Pages(ctx, func(page *compute.MachineTypeList) error {
		for _, mt := range page.Items {
			types = append(types, mt.Name)
		}
		return nil
	})
	return types, err
}

func (g *GCPProvider) RecommendedType() string {
	return "e2-micro"
}

func (g *GCPProvider) CreateInstance(ctx context.Context, name, zone, machineType string) (string, string, error) {
	instance := &compute.Instance{
		Name: name,
		MachineType: fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType),
		Disks: []*compute.AttachedDisk{
			{
				Boot: true,
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: "projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts",
				},
			},
		},
		NetworkInterfaces: []*compute.NetworkInterface{
			{
				AccessConfigs: []*compute.AccessConfig{{Type: "ONE_TO_ONE_NAT"}},
			},
		},
	}

	maxRetries := 5
	for attempt := range maxRetries {
		op, err := g.service.Instances.Insert(g.project, zone, instance).Do()
		if err == nil {
			for {
				operation, err := g.service.ZoneOperations.Get(g.project, zone, op.Name).Context(ctx).Do()
				if err != nil {
					return "", "", fmt.Errorf("failed to check operation status: %v", err)
				}
				if operation.Status == "DONE" {
					if operation.Error != nil {
						return "", "", fmt.Errorf("operation failed: %v", operation.Error)
					}
					break
				}
				fmt.Printf("Waiting for instance creation (%s)...\n", operation.Status)
				time.Sleep(2 * time.Second)
			}

			instanceInfo, err := g.service.Instances.Get(g.project, zone, name).Context(ctx).Do()
			if err != nil {
				return "", "", fmt.Errorf("failed to get instance info: %v", err)
			}
			ip := instanceInfo.NetworkInterfaces[0].AccessConfigs[0].NatIP
			return name, ip, nil
		}

		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code >= 500 {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			fmt.Printf("Create retryable error: (%d/%d): %v, waiting %v\n", attempt+1, maxRetries, err, wait)
			time.Sleep(wait)
			continue
		}
		return "", "", fmt.Errorf("non-retryable error: %v", err)
	}
	return "", "", fmt.Errorf("failed to create instance after %d retries", maxRetries)
}

func (g *GCPProvider) DeleteInstance(ctx context.Context, zone, instanceID string) error {
	fmt.Printf("Attempting to delete instance %s in zone %s\n", instanceID, zone)
	maxRetries := 5
	for attempt := range maxRetries {
		op, err := g.service.Instances.Delete(g.project, zone, instanceID).Context(ctx).Do()
		if err == nil {
			for {
				operation, err := g.service.ZoneOperations.Get(g.project, zone, op.Name).Context(ctx).Do()
				if err != nil {
					return fmt.Errorf("failed to check delete operation status: %v", err)
				}
				if operation.Status == "DONE" {
					if operation.Error != nil {
						return fmt.Errorf("delete operation failed: %v", operation.Error)
					}
					fmt.Printf("Instance %s deleted successfully\n", instanceID)
					return nil
				}
				fmt.Printf("Waiting for instance deletion (%s)...\n", operation.Status)
				time.Sleep(2 * time.Second)
			}
		}

		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code >= 500 {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			fmt.Printf("Delete retryable error (%d/%d): %v, waiting %v, waiting %v\n", attempt+1, maxRetries, err, wait, wait)
			time.Sleep(wait)
			continue
		}
		return fmt.Errorf("non-retryable error: %v", err)
	}
	return fmt.Errorf("failed to delete instance after %d retries", maxRetries)
}

func (g *GCPProvider) DeleteDisk(ctx context.Context, zone, diskID string) error {
	fmt.Printf("attempting to delete disk %s in zone %s\n", diskID, zone)
	maxRetries := 5
	for attempt := range maxRetries {
		op, err := g.service.Disks.Delete(g.project, zone, diskID).Context(ctx).Do()
		if err == nil {
			for {
				operation, err := g.service.ZoneOperations.Get(g.project, zone, op.Name).Context(ctx).Do()
				if err != nil {
					return fmt.Errorf("failed to check disk delete operation status: %v", err)
				}
				if operation.Status == "DONE" {
					if operation.Error != nil {
						return fmt.Errorf("disk delete operation failed: %v", operation.Error)
					}
					fmt.Printf("Disk %s deleted successfully\n", diskID)
					return nil 
				}
				fmt.Printf("Waiting for disk deletion (%s)...\n", operation.Status)
				time.Sleep(2 * time.Second)
			}
		}	
		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code >= 500 {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			fmt.Printf("Disk delete retryable error (%d/%d): %v, waiting %v, waiting %v\n", attempt+1, maxRetries, err, wait, wait)
			time.Sleep(wait)
			continue
		}
		return fmt.Errorf("non-retryable error deleteing disk: %v", err)
	}
	return fmt.Errorf("failed to delete disk after %d retries", maxRetries)
}

func (g *GCPProvider) GetInstanceInfo(ctx context.Context, zone, instanceID string) (InstanceInfo, error) {
    instance, err := g.service.Instances.Get(g.project, zone, instanceID).Context(ctx).Do()
    if err != nil {
        return InstanceInfo{}, fmt.Errorf("failed to get instance info: %v", err)
    }

    var info InstanceInfo
    info.IP = instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
    for _, disk := range instance.Disks {
        if disk.Boot {
            parts := strings.Split(disk.Source, "/")
            info.DiskID = parts[len(parts)-1]
            break
        }
    }
    if info.DiskID == "" {
        return InstanceInfo{}, fmt.Errorf("no boot disk found for instance %s", instanceID)
    }
    return info, nil
}