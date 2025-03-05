package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type ProxyDeployer interface {
	Deploy(ip string) error
}

type AnsibleProxyDeployer struct {
	user string
	keyPath  string
}

func NewAnsibleProxyDeployer(user, keyPath string) *AnsibleProxyDeployer {
	return &AnsibleProxyDeployer{user: user, keyPath: keyPath}
}

func (d *AnsibleProxyDeployer) Deploy(ip string) error {
	invetory := fmt.Sprintf("[proxy_server]\n%s ansible_user=%s ansible_ssh_private_key_file=%s", ip, d.user, d.keyPath)
	if err := os.WriteFile("inventory.ini", []byte(invetory), 0645); err != nil {
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
		return nil
	}
	defer os.Remove("playbook.yml")

	fmt.Println("Waiting for SSH to be ready...")
	for i := 0; i < 30; i++ {
		cmd := exec.Command("ssh", "-i", d.keyPath, "-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", d.user, ip), "exit")
		if err := cmd.Run(); err == nil {
			break
		}
		fmt.Printf("SSH not ready, retrying in 2 seconds (%d/30)...\n", i+1)
		time.Sleep(2 * time.Second)
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
			fmt.Println("ERROR: ", scanner.Text())
		}
	}()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ansible-playbook failed: %v", err)
	}

	fmt.Println("Ansible playbook execution completed successfully.")
	return nil
}