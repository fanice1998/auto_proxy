package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/joho/godotenv"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type ProxyRecord struct {
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Region     string `json:"region"`
	Zone       string `json:"zone"`
	InstanceID string `json:"instance_id"`
	IP         string `json:"ip"`
}

type GCPProvider struct {
	service *compute.Service
	project string
}

var logger *log.Logger

func init() {
	file, err := os.OpenFile("proxy_error.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println("Failed to open log file:", err)
		os.Exit(1)
	}
	logger = log.New(file, "Proxy: ", log.LstdFlags)
}

func loadEnv() error {
	err := godotenv.Load()
	if err != nil {
		return fmt.Errorf("error loading .env file: %v", err)
	}
	return nil
}

func NewGCPProvider(project string) (*GCPProvider, error) {
	if err := loadEnv(); err != nil {
		return nil, err
	}
	ctx := context.Background()
	credsPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if credsPath == "" {
		return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS not set in .env")
	}
	svc, err := compute.NewService(ctx, option.WithCredentialsFile(credsPath))
	if err != nil {
		return nil, err
	}
	return &GCPProvider{service: svc, project: project}, nil
}

func (g *GCPProvider) ListRegions() ([]string, error) {
	req := g.service.Regions.List(g.project)
	regions := []string{}
	if err := req.Pages(context.Background(), func(page *compute.RegionList) error {
		for _, region := range page.Items {
			regions = append(regions, region.Name)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return regions, nil
}

func (g *GCPProvider) ListZones(region string) ([]string, error) {
	req := g.service.Zones.List(g.project)
	zones := []string{}
	if err := req.Pages(context.Background(), func(page *compute.ZoneList) error {
		for _, zone := range page.Items {
			if strings.HasPrefix(zone.Name, region) {
				zones = append(zones, zone.Name)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return zones, nil
}

func (g *GCPProvider) ListMachineTypes(zone string) ([]string, error) {
	req := g.service.MachineTypes.List(g.project, zone)
	types := []string{}
	if err := req.Pages(context.Background(), func(page *compute.MachineTypeList) error {
		for _, mt := range page.Items {
			types = append(types, mt.Name)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return types, nil
}

func (g *GCPProvider) RecommendedType() string {
	return "e2-micro"
}

func (g *GCPProvider) CreateInstance(name, zone, machineType string) (string, string, error) {
	instance := &compute.Instance{
		Name:        name,
		MachineType: fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType),
		Disks: []*compute.AttachedDisk{
			{
				Boot: true,
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: "projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts", // 修改為 Ubuntu 22.04 LTS
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
	for attempt := 0; attempt < maxRetries; attempt++ {
		op, err := g.service.Instances.Insert(g.project, zone, instance).Do()
		if err == nil {
			for {
				operation, err := g.service.ZoneOperations.Get(g.project, zone, op.Name).Do()
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

			instanceInfo, err := g.service.Instances.Get(g.project, zone, name).Do()
			if err != nil {
				return "", "", fmt.Errorf("failed to get instance info: %v", err)
			}
			ip := instanceInfo.NetworkInterfaces[0].AccessConfigs[0].NatIP
			return name, ip, nil
		}

		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code >= 500 {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			msg := fmt.Sprintf("Create retryable error (%d/%d): %v, waiting %v", attempt+1, maxRetries, err, wait)
			logger.Println(msg)
			fmt.Println(msg)
			time.Sleep(wait)
			continue
		}
		logger.Printf("Create non-retryable error: %v", err)
		return "", "", fmt.Errorf("non-retryable error: %v", err)
	}
	logger.Printf("Failed to create instance %s after %d retries", name, maxRetries)
	return "", "", fmt.Errorf("failed to create instance after %d retries", maxRetries)
}

func (g *GCPProvider) DeleteInstance(zone, instanceID string) error {
	fmt.Printf("Attempting to delete instance %s in zone %s\n", instanceID, zone)

	// Step 1: 获取实例信息以确定磁盘名称
	instance, err := g.service.Instances.Get(g.project, zone, instanceID).Do()
	if err != nil {
		return fmt.Errorf("failed to get instance %s for disk info: %v", instanceID, err)
	}

	// 獲取磁盤名稱
	var bootDisk string
	for _, disk := range instance.Disks {
		if disk.Boot {
			// 磁盤完整路徑名稱 "projects/<project>/zones/<zone>/disks/<disk-name>"
			// 只獲取磁盤名稱
			parts := strings.Split(disk.Source, "/")
			bootDisk = parts[len(parts)-1]
			break
		}
	}
	if bootDisk == "" {
		return fmt.Errorf("no boot disk found for instance %s", instanceID)
	}
	fmt.Printf("Found boot disk: %s\n", bootDisk)

	// Step 2: 删除實例
	maxRetries := 5
	for attempt := range maxRetries {
		op, err := g.service.Instances.Delete(g.project, zone, instanceID).Do()
		if err == nil {
			for {
				operation, err := g.service.ZoneOperations.Get(g.project, zone, op.Name).Do()
				if err != nil {
					return fmt.Errorf("failed to check delete operation status: %v", err)
				}
				if operation.Status == "DONE" {
					if operation.Error != nil {
						return fmt.Errorf("delete operation failed: %v", operation.Error)
					}
					fmt.Printf("Instance %s deleted successfully\n", instanceID)
					break
				}
				fmt.Printf("Waiting for instance deletion (%s)...\n", operation.Status)
				time.Sleep(2 * time.Second)
			}
			// 實例刪除成功，退出 re-try 循環
			break
		}

		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code >= 500 {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			msg := fmt.Sprintf("Instance delete retryable error (%d/%d): %v, waiting %v", attempt+1, maxRetries, err, wait)
			logger.Println(msg)
			fmt.Println(msg)
			time.Sleep(wait)
			continue
		}
		logger.Printf("Delete non-retryable error: %v", err)
		return fmt.Errorf("non-retryable error: %v", err)
	}

	// Step 3: 刪除啟動磁盤
	for attempt := range maxRetries {
		op, err := g.service.Disks.Delete(g.project, zone, bootDisk).Do()
		if err == nil {
			for {
				operation, err := g.service.ZoneOperations.Get(g.project, zone, op.Name).Do()
				if err != nil {
					return fmt.Errorf("failed to check disk delete operation status: %v", err)
				}
				if operation.Status == "DONE" {
					if operation.Error != nil {
						return fmt.Errorf("disk delete operation failed: %v", operation.Error)
					}
					fmt.Printf("Boot disk %s deleted successfully\n", bootDisk)
					return nil
				}
				fmt.Printf("Waiting for boot disk deletion (%s)...\n", operation.Status)
				time.Sleep(2 * time.Second)
			}
		}
		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code >= 500 {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			msg := fmt.Sprintf("Disk delete retryable error (%d/%d): %v, waiting %v", attempt+1, maxRetries, err, wait)
			logger.Println(msg)
			fmt.Println(msg)
			time.Sleep(wait)
			continue
		}
		logger.Printf("Disk delete non-retryable error: %v", err)
		return fmt.Errorf("non-retryable error: %v", err)
	}
	logger.Printf("Failed to delete instance %s after %d retries", instanceID, maxRetries)
	return fmt.Errorf("failed to delete instance after %d retries", maxRetries)
}

func loadRecords() ([]ProxyRecord, error) {
	data, err := os.ReadFile("proxy_records.json")
	if os.IsNotExist(err) {
		return []ProxyRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var records []ProxyRecord
	json.Unmarshal(data, &records)
	return records, nil
}

func saveRecords(records []ProxyRecord) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile("proxy_records.json", data, 0644)
}

func deployProxy(ip string) error {
	inventory := fmt.Sprintf("[proxy_server]\n%s ansible_user=fanice ansible_ssh_private_key_file=/home/fanice/.ssh/faniceNP", ip) // Ubuntu 預設使用者為 "ubuntu"
	if err := os.WriteFile("inventory.ini", []byte(inventory), 0644); err != nil {
		return err
	}
	defer os.Remove("inventory.ini")

	playbook := `
- name: Deploy Shadowsocks Proxy Server on Ubuntu
  hosts: proxy_server
  become: yes
  vars:
    ansible_ssh_common_args: '-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null'
  tasks:
    - name: Wait for SSH to be ready
      wait_for:
        port: 22
        host: "{{ ansible_host }}"
        state: started
        timeout: 30
    - name: Update apt cache
      apt:
        update_cache: yes
    - name: Install Shadowsocks-libev
      apt:
        name: shadowsocks-libev
        state: present
    - name: Create Shadowsocks config directory
      file:
        path: /etc/shadowsocks-libev
        state: directory
        mode: '0755'
    - name: Configure Shadowsocks
      copy:
        content: |
          {
              "server": "0.0.0.0",
              "server_port": 8388,
              "password": "s;980303",
              "timeout": 300,
              "method": "aes-256-gcm",
              "fast_open": true
          }
        dest: /etc/shadowsocks-libev/config.json
      notify: Restart Shadowsocks
    - name: Ensure Shadowsocks service is enabled and started
      systemd:
        name: shadowsocks-libev
        enabled: yes
        state: started
    - name: Install and configure UFW
      block:
        - name: Install UFW
          apt:
            name: ufw
            state: present
        - name: Allow SSH
          ufw:
            rule: allow
            port: 22
        - name: Allow Shadowsocks port
          ufw:
            rule: allow
            port: 8388
        - name: Enable UFW
          ufw:
            state: enabled
  handlers:
    - name: Restart Shadowsocks
      systemd:
        name: shadowsocks-libev
        state: restarted
`
	if err := os.WriteFile("playbook.yml", []byte(playbook), 0644); err != nil {
		return err
	}
	defer os.Remove("playbook.yml")

	// Wait for SSH dynamically
	fmt.Println("Waiting for SSH to be ready...")
	err := waitForSSH(ip, 22, 60*time.Second)
	if err != nil {
		return fmt.Errorf("SSH not ready: %v", err)
	}

	fmt.Println("Starting Ansible playbook execution...")
	cmd := exec.Command("ansible-playbook", "-i", "inventory.ini", "playbook.yml", "-v", "-e", "ansible_ssh_common_args='-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null'")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ansible-playbook: %v", err)
	}

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Println("ERROR:", scanner.Text())
		}
	}()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ansible-playbook failed: %v", err)
	}

	fmt.Println("Ansible playbook execution completed successfully.")
	return nil
}

func waitForSSH(host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("SSH not ready after %s", timeout)
}

func loadMappings(filePath string) (map[string]string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var mappings map[string]string
	if err := json.Unmarshal(data, &mappings); err != nil {
		return nil, err
	}

	return mappings, nil
}

func regionsToLocations(regions []string, mappings map[string]string) []string {
	locations := make([]string, 0, len(regions))
	for _, region := range regions {
		if location, ok := mappings[region]; ok {
			locations = append(locations, location)
		} else {
			locations = append(locations, region)
		}
	}
	return locations
}

func main() {
	createCmd := flag.NewFlagSet("create", flag.ExitOnError)
	deleteCmd := flag.NewFlagSet("delete", flag.ExitOnError)
	listCmd := flag.NewFlagSet("list", flag.ExitOnError)
	deleteName := deleteCmd.String("name", "", "Name of the proxy to delete")

	if len(os.Args) < 2 {
		fmt.Println("Usage: auto_proxy [create|delete|list]")
		return
	}

	provider, err := NewGCPProvider("flash-gasket-451912-a8")
	if err != nil {
		fmt.Println("Error initializing GCP:", err)
		logger.Printf("Error initializing GCP: %v", err)
		return
	}

	switch os.Args[1] {
	case "create":
		createCmd.Parse(os.Args[2:])

		platforms := []string{"GCP"}
		var selectedPlatform string
		survey.AskOne(&survey.Select{Message: "Choose a cloud platform:", Options: platforms}, &selectedPlatform)

		regions, err := provider.ListRegions()
		if err != nil {
			fmt.Println("Error listing regions:", err)
			logger.Printf("Error listing regions: %v", err)
			return
		}
		var selectedRegion string
		gcp_locations, err := loadMappings("./gcp_region_map.json")
		if err != nil {
			fmt.Println("Error loading region mappings:", err)
			logger.Printf("Error loading region mappings: %v", err)
			return
		}
		locations := regionsToLocations(regions, gcp_locations)
		survey.AskOne(&survey.Select{Message: "Choose a region:", Options: locations}, &selectedRegion)

		reverseMap := make(map[string]string)
		for k, v := range gcp_locations {
			reverseMap[v] = k
		}
		// zones, err := provider.ListZones(selectedRegion)
		zones, err := provider.ListZones(reverseMap[selectedRegion])
		if err != nil {
			fmt.Println("Error listing zones:", err)
			logger.Printf("Error listing zones: %v", err)
			return
		}
		var selectedZone string
		survey.AskOne(&survey.Select{Message: "Choose a zone:", Options: zones}, &selectedZone)

		machineTypes, err := provider.ListMachineTypes(selectedZone)
		if err != nil {
			fmt.Println("Error listing machine types:", err)
			logger.Printf("Error listing machine types: %v", err)
			return
		}
		recommended := provider.RecommendedType()
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
		instanceID, ip, err := provider.CreateInstance(name, selectedZone, selectedType)
		if err != nil {
			fmt.Println("Error creating instance:", err)
			return
		}

		if err := deployProxy(ip); err != nil {
			fmt.Println("Error deploying proxy:", err)
			logger.Printf("Error deploying proxy %s: %v", name, err)
			return
		}

		records, _ := loadRecords()
		records = append(records, ProxyRecord{
			Name:       name,
			Provider:   "gcp",
			Region:     selectedRegion,
			Zone:       selectedZone,
			InstanceID: instanceID,
			IP:         ip,
		})
		saveRecords(records)
		fmt.Printf("Shadowsocks proxy created at: %s:8388\n - Protocol: Shadowsocks\n - Password: s;980303\n - Encryption: aes-256-gcm\n", ip)

	case "delete":
		if len(os.Args) < 3 { // 檢查是否至少有 "delete" 和一個參數
			fmt.Println("Error: Invalid delete command format.")
			fmt.Println("Usage: auto_proxy delete -name <proxy-name>")
			fmt.Println("Example: auto_proxy delete -name proxy-us-central1a")
			fmt.Println("To see available proxies, run: auto_proxy list")
			return
		}

		deleteCmd.Parse(os.Args[2:])
		if *deleteName == "" { // 檢查 -name 是否有值
			fmt.Println("Error: Proxy name is required")
			fmt.Println("Usage: auto_proxy delete -name <proxy-name>")
			fmt.Println("Example: auto_proxy delete -name proxy-us-central1a")
			fmt.Println("To see available proxies, run: auto_proxy list")
			return
		}

		records, _ := loadRecords()
		for i, r := range records {
			if r.Name == *deleteName {
				if err := provider.DeleteInstance(r.Zone, r.InstanceID); err != nil {
					fmt.Println("Error deleting instance:", err)
					return
				}
				records = append(records[:i], records[i+1:]...)
				saveRecords(records)
				fmt.Println("Proxy deleted:", *deleteName)
				return
			}
		}
		fmt.Println("Proxy not found:", *deleteName)

	case "list":
		listCmd.Parse(os.Args[2:])
		records, _ := loadRecords()
		if len(records) == 0 {
			fmt.Println("No proxies found.")
			return
		}
		for _, r := range records {
			fmt.Printf("Name: %s, IP: %s, Region: %s, Zone: %s\n", r.Name, r.IP, r.Region, r.Zone)
		}
	}
}
