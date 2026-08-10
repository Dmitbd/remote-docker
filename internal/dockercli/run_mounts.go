package dockercli

import (
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
)

var runValueOptions = map[string]bool{
	"--add-host": true, "--annotation": true, "--attach": true, "-a": true,
	"--blkio-weight": true, "--cap-add": true, "--cap-drop": true,
	"--cgroup-parent": true, "--cidfile": true, "--cpu-period": true,
	"--cpu-quota": true, "--cpu-shares": true, "-c": true, "--cpuset-cpus": true,
	"--cpuset-mems": true, "--device": true, "--device-cgroup-rule": true,
	"--device-read-bps": true, "--device-read-iops": true, "--device-write-bps": true,
	"--device-write-iops": true, "--dns": true, "--dns-option": true,
	"--dns-search": true, "--domainname": true, "--entrypoint": true,
	"--env": true, "-e": true, "--env-file": true, "--expose": true,
	"--gpus": true, "--group-add": true, "--health-cmd": true,
	"--health-interval": true, "--health-retries": true, "--health-start-interval": true,
	"--health-start-period": true, "--health-timeout": true, "--hostname": true,
	"-h": true, "--init-path": true, "--ip": true, "--ip6": true,
	"--ipc": true, "--isolation": true, "--label": true, "-l": true,
	"--label-file": true, "--link": true, "--link-local-ip": true,
	"--log-driver": true, "--log-opt": true, "--mac-address": true,
	"--memory": true, "-m": true, "--memory-reservation": true,
	"--memory-swap": true, "--memory-swappiness": true, "--mount": true,
	"--name": true, "--network": true, "--net": true, "--network-alias": true,
	"--oom-score-adj": true, "--pid": true, "--pids-limit": true,
	"--platform": true, "--publish": true, "-p": true, "--pull": true,
	"--restart": true, "--runtime": true, "--security-opt": true,
	"--shm-size": true, "--stop-signal": true, "--stop-timeout": true,
	"--storage-opt": true, "--sysctl": true, "--tmpfs": true,
	"--ulimit": true, "--user": true, "-u": true, "--userns": true,
	"--uts": true, "--volume": true, "-v": true, "--volume-driver": true,
	"--volumes-from": true, "--workdir": true, "-w": true,
}

func analyzeRunArgs(args []string) ([]string, []Port, []Reason) {
	var binds []string
	var ports []Port
	var unsupported []Reason
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" || argument == "" || argument[0] != '-' {
			break
		}

		name, value, hasInlineValue := splitOption(argument)
		if !hasInlineValue && runValueOptions[name] && index+1 < len(args) {
			index++
			value = args[index]
		}
		switch name {
		case "-v", "--volume":
			if source, ok := parseVolumeSource(value); ok {
				binds = append(binds, source)
			}
		case "--mount":
			if source, ok := parseMountSource(value); ok {
				binds = append(binds, source)
			}
		case "-p", "--publish":
			port, fixed, reason := parsePublishedPort(value)
			if reason != nil {
				unsupported = append(unsupported, *reason)
			} else if fixed {
				ports = append(ports, port)
			}
		case "--network", "--net":
			if strings.EqualFold(value, "host") {
				unsupported = append(unsupported, Reason{Code: ReasonHostNetworking, Detail: "host networking cannot be forwarded from WSL2"})
			}
		}
	}
	return binds, ports, unsupported
}

func splitOption(argument string) (name, value string, inline bool) {
	if separator := strings.IndexByte(argument, '='); separator >= 0 {
		return argument[:separator], argument[separator+1:], true
	}
	if strings.HasPrefix(argument, "-v") && len(argument) > 2 && !strings.HasPrefix(argument, "--") {
		return "-v", argument[2:], true
	}
	if strings.HasPrefix(argument, "-p") && len(argument) > 2 && !strings.HasPrefix(argument, "--") {
		return "-p", argument[2:], true
	}
	return argument, "", false
}

func parseVolumeSource(specification string) (string, bool) {
	separator := strings.IndexByte(specification, ':')
	if separator <= 0 {
		return "", false
	}
	source := specification[:separator]
	if isNamedVolume(source) {
		return "", false
	}
	return source, true
}

func isNamedVolume(source string) bool {
	if source == "" || source == "." || source == ".." || strings.ContainsAny(source, `/\\`) {
		return false
	}
	for index, character := range source {
		alphaNumeric := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
		if index == 0 && !alphaNumeric {
			return false
		}
		if !alphaNumeric && character != '_' && character != '.' && character != '-' {
			return false
		}
	}
	return true
}

func parseMountSource(specification string) (string, bool) {
	reader := csv.NewReader(strings.NewReader(specification))
	reader.TrimLeadingSpace = true
	fields, err := reader.Read()
	if err != nil {
		return "", false
	}
	mountType := ""
	source := ""
	for _, field := range fields {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "type":
			mountType = strings.ToLower(strings.TrimSpace(value))
		case "src", "source":
			source = value
		}
	}
	return source, mountType == "bind" && source != ""
}

func parsePublishedPort(specification string) (Port, bool, *Reason) {
	protocol := "tcp"
	if slash := strings.LastIndexByte(specification, '/'); slash >= 0 {
		protocol = strings.ToLower(specification[slash+1:])
		specification = specification[:slash]
	}
	if protocol != "tcp" {
		return Port{}, false, &Reason{Code: ReasonUnsupportedUDP, Detail: "only TCP port forwarding is supported: " + protocol}
	}

	lastColon := strings.LastIndexByte(specification, ':')
	if lastColon < 0 {
		if _, err := parsePortNumber(specification); err != nil {
			return Port{}, false, &Reason{Code: ReasonInvalidPort, Detail: err.Error()}
		}
		return Port{}, false, nil
	}
	containerPort, err := parsePortNumber(specification[lastColon+1:])
	if err != nil {
		return Port{}, false, &Reason{Code: ReasonInvalidPort, Detail: err.Error()}
	}
	left := specification[:lastColon]
	hostIP := ""
	hostPortText := left
	if separator := strings.LastIndexByte(left, ':'); separator >= 0 {
		hostIP = strings.Trim(left[:separator], "[]")
		hostPortText = left[separator+1:]
	}
	hostPort, err := parsePortNumber(hostPortText)
	if err != nil {
		return Port{}, false, &Reason{Code: ReasonInvalidPort, Detail: err.Error()}
	}
	if hostPort == 0 {
		return Port{}, false, nil
	}
	return Port{HostIP: hostIP, HostPort: hostPort, ContainerPort: containerPort}, true, nil
}

func parsePortNumber(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 0 || port > 65535 {
		return 0, fmt.Errorf("invalid published port %q", value)
	}
	return port, nil
}
