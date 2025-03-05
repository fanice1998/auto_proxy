package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/joho/godotenv"
)

type Commander struct {
	provider      CloudProvider
	deployer      ProxyDeployer
	recordManager *RecordManager
	logger        *log.Logger
}

func NewCommander(provider CloudProvider, deployer ProxyDeployer, recordManager *RecordManager, logger *log.Logger) *Commander {
	return &Commander{
		provider:      provider,
		deployer:      deployer,
		recordManager: recordManager,
		logger:        logger,
	}
}

func (c *Commander) Create(ctx context.Context) error {
	platforms := []string{"GCP"}
	var selectedPlatform string
	survey.AskOne(&survey.Select{Message: "Choose a cloud platform:", Options: platforms}, &selectedPlatform)

	regions, err := c.provider.ListRegions(ctx)
	if err != nil {
		return fmt.Errorf("error listing regions: %v", err)
	}

	var selectedRegion, selectedLocation string
	var locations []string
	// 依照不同的 platform 回傳不同的 location 列表
	switch strings.ToUpper(selectedPlatform) {
	case "GCP":
		locations = regionToLocations(regions, gcp_locations)
	default:
		return fmt.Errorf("invalid platform: %s", selectedPlatform)
	}
	survey.AskOne(&survey.Select{Message: "Choose a region:", Options: locations}, &selectedLocation)
	reverseMap := make(map[string]string)
	for k, v := range gcp_locations {
		reverseMap[v] = k
	}
	selectedRegion = reverseMap[selectedLocation]

	zones, err := c.provider.ListZones(ctx, selectedRegion)
	if err != nil {
		return fmt.Errorf("error listing zones: %v", err)
	}
	var selectedZone string
	survey.AskOne(&survey.Select{Message: "Choose a zone:", Options: zones}, &selectedZone)

	machineTypes, err := c.provider.ListMachineTypes(ctx, selectedZone)
	if err != nil {
		return fmt.Errorf("error listing machine types: %v", err)
	}
	recommended := c.provider.RecommendedType()
	for i, mt := range machineTypes {
		if mt == recommended {
			machineTypes[i] = mt + " (recommended)"
		}
	}
	var selectedType string
	survey.AskOne(&survey.Select{Message: "Choose a machine type:", Options: machineTypes}, &selectedType)
	if strings.HasSuffix(selectedType, " (recommended)") {
		selectedType = recommended
	}

	name := "proxy-" + strings.ReplaceAll(selectedZone, "-", "")
	instanceID, ip, err := c.provider.CreateInstance(ctx, name, selectedZone, selectedType)
	if err != nil {
		return fmt.Errorf("error creating instance: %v", err)
	}

	if err := c.deployer.Deploy(ip); err != nil {
		c.logger.Printf("Error deploying proxy %s: %v", name, err)
		return fmt.Errorf("error deploying proxy: %v", err)
	}

	records, err := c.recordManager.Load()
	if err != nil {
		return fmt.Errorf("error loading records: %v", err)
	}
	records = append(records,
		ProxyRecord{
			Name:       name,
			Provider:   "gcp",
			Region:     selectedRegion,
			Zone:       selectedZone,
			InstanceID: instanceID,
			IP:         ip,
			Type: "instance",
			Location:   selectedLocation,
		})
	if err := c.recordManager.Save(records); err != nil {
		return fmt.Errorf("error saving records: %v", err)
	}

	fmt.Printf("Shadowsocks proxy created at: %s:8388\n - Protocol: Shadowsocks\n - Password: s;980303\n - Encryption: aes-256-gcm\n", ip)
	return nil
}

func (c *Commander) Delete(ctx context.Context, name string) error {
	records, err := c.recordManager.Load()
	if err != nil {
		return fmt.Errorf("error loading records: %v", err)
	}

	var instanceRecord *ProxyRecord
	for i, r := range records {
		if r.Name == name && r.Type == "instance" {
			instanceRecord = &records[i]
			break
		}
	}

	if instanceRecord == nil {
		fmt.Printf("Proxy not found: %s\n", name)
		return nil
	}

	// 獲取實例信息
	info, err := c.provider.GetInstanceInfo(ctx, instanceRecord.Zone, instanceRecord.InstanceID)
	if err != nil {
		c.logger.Printf("Failed to get instance info for %s: %v", instanceRecord.InstanceID, err)
	}else {
		fmt.Printf("Found boot disk: %s for instance %s\n", info.DiskID, instanceRecord.InstanceID)
	}

	// 刪除 Instance
	if err := c.provider.DeleteInstance(ctx, instanceRecord.Zone, instanceRecord.InstanceID); err != nil {
		c.logger.Printf("Error deleting instance %s: %v", instanceRecord.InstanceID, err)
		fmt.Printf("Failed to delete instance %s\n", instanceRecord.InstanceID)
		return nil
	}

	for i, r := range records {
		if r.Name == name && r.Type == "instance" {
			records = append(records[:i], records[i+1:]...)
			break
		}
	}

	// 刪除磁碟
	if info.DiskID != "" {
		diskRecord := ProxyRecord{
			Name: name,
			Provider: instanceRecord.Provider,
			Region: instanceRecord.Region,
			Zone: instanceRecord.Zone,
			InstanceID: info.DiskID,
			Type: "disk",
			Location: instanceRecord.Location,
		}

		if err := c.provider.DeleteDisk(ctx, instanceRecord.Zone, info.DiskID); err != nil {
			c.logger.Printf("Error deleting disk %s: %v", info.DiskID, err)
			fmt.Printf("Failed to delete disk %s\n", info.DiskID)
			// 如果刪除失敗，則添加到紀錄
			records = append(records, diskRecord)
		}
	}

	if err := c.recordManager.Save(records); err != nil {
		return fmt.Errorf("error saving records: %v", err)
	}

	fmt.Printf("Proxy %s deleted.\n", name)
	return nil
}

func (c *Commander) List() error {
	records, err := c.recordManager.Load()
	if err != nil {
		return fmt.Errorf("error loading records: %v", err)
	}
	if len(records) == 0 {
		fmt.Println("No proxies found.")
		return nil
	}
	for _, r := range records {
		fmt.Printf("Name: %s, IP: %s, Region: %s, Location: %s\n", r.Name, r.IP, r.Region, r.Location)
	}
	return nil
}

func main() {
	logger := log.New(os.Stdout, "Proxy: ", log.LstdFlags)

	if err := godotenv.Load(); err != nil {
		logger.Printf("Error loading .env file: %v", err)
	}

	credsPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if credsPath == "" {
		logger.Println("GOOGLE_APPLICATION_CREDENTIALS not set in .env")
		os.Exit(1)
	}

	provider, err := NewGCPProvider("flash-gasket-451912-a8", credsPath)
	if err != nil {
		logger.Printf("Error initializing GCP: %v", err)
		os.Exit(1)
	}

	deployer := NewAnsibleProxyDeployer("fanice", "/home/fanice/.ssh/faniceNP")
	recordManager := NewRecordManager("proxy_records.json")
	commander := NewCommander(provider, deployer, recordManager, logger)

	createCmd := flag.NewFlagSet("create", flag.ExitOnError)
	deleteCmd := flag.NewFlagSet("delete", flag.ExitOnError)
	listCmd := flag.NewFlagSet("list", flag.ExitOnError)
	deleteName := deleteCmd.String("name", "", "Name of the proxy to delete")

	if len(os.Args) < 2 {
		fmt.Println("Usage: auto_proxy [create|delete|list]")
		return
	}

	ctx := context.Background()
	switch os.Args[1] {
	case "create":
		createCmd.Parse(os.Args[2:])
		if err := commander.Create(ctx); err != nil {
			fmt.Println(err)
		}
	case "delete":
		deleteCmd.Parse(os.Args[2:])
		if *deleteName == "" {
			fmt.Println("Error: Proxy name is required. Usage: auto_proxy delete -name <proxy-name>")
			return
		}
		if err := commander.Delete(ctx, *deleteName); err != nil {
			fmt.Println(err)
		}
	case "list":
		listCmd.Parse(os.Args[2:])
		if err := commander.List(); err != nil {
			fmt.Println(err)
		}
	default:
		fmt.Println("Unknown command:", os.Args[1])
		fmt.Println("Usage: auto_proxy [create|delete|list]")
	}
}

func regionToLocations(regions []string, mapping map[string]string) []string {
	locations := make([]string, 0)
	for _, r := range regions {
		if location, ok := mapping[r]; ok {
			locations = append(locations, location)
		} else {
			locations = append(locations, r)
		}
	}
	return locations
}
