// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package release updates release documentation from chart references and renders.
package release

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type imageSet map[string]map[string]bool

var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func (images imageSet) validate() error {
	for image := range images {
		if strings.ContainsAny(image, " \t\r\n`|\"'") {
			return fmt.Errorf("invalid rendered image reference %q", image)
		}
		name, digest, hasDigest := strings.Cut(image, "@")
		if hasDigest && (digest == "" || !strings.Contains(digest, ":")) {
			return fmt.Errorf("invalid rendered image digest in %q", image)
		}
		if separator := strings.LastIndex(name, ":"); separator > strings.LastIndex(name, "/") {
			if !imageTagPattern.MatchString(name[separator+1:]) {
				return fmt.Errorf("invalid rendered image tag in %q", image)
			}
			name = name[:separator]
		}
		if !strings.Contains(name, "/") || strings.Contains(name, "//") || strings.HasSuffix(name, "/") {
			return fmt.Errorf("rendered image reference has no valid repository: %q", image)
		}
	}
	return nil
}

func (images imageSet) add(image, use string) {
	image = fullReference(image)
	if images[image] == nil {
		images[image] = make(map[string]bool)
	}
	images[image][use] = true
}

// fullReference expands Docker Hub shorthand without looking up registry data.
func fullReference(image string) string {
	name, digest, hasDigest := strings.Cut(image, "@")
	hasTag := strings.LastIndex(name, ":") > strings.LastIndex(name, "/")
	first, _, hasPath := strings.Cut(name, "/")
	if !hasPath || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		name = "docker.io/" + name
	}
	if rest, ok := strings.CutPrefix(name, "index.docker.io/"); ok {
		name = "docker.io/" + rest
	}
	if rest, ok := strings.CutPrefix(name, "docker.io/"); ok && !strings.Contains(rest, "/") {
		name = "docker.io/library/" + rest
	}
	if !hasTag && !hasDigest {
		name += ":latest"
	}
	if hasDigest {
		name += "@" + digest
	}
	return name
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func at(value map[string]any, path ...string) any {
	var result any = value
	for _, key := range path {
		result = object(result)[key]
	}
	return result
}

func text(value any) string {
	result, _ := value.(string)
	return result
}

// discover inventories effective images, including Operator defaults only when a
// Collector actually relies on them. Collector configuration is never traversed.
func discover(data []byte) (imageSet, error) {
	images := make(imageSet)
	defaults := make(map[string]string)
	var collectors []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var resource map[string]any
		if err := decoder.Decode(&resource); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode rendered manifest: %w", err)
		}
		if resource == nil {
			continue
		}
		kind := text(resource["kind"])
		use := "Default workload"
		hooks := text(at(resource, "metadata", "annotations", "helm.sh/hook"))
		if hooks != "" {
			runsOnInstall := false
			for _, hook := range strings.Split(hooks, ",") {
				switch strings.TrimSpace(hook) {
				case "pre-install", "post-install":
					runsOnInstall = true
				}
			}
			if !runsOnInstall {
				continue
			}
			use = "Helm hook: " + hooks
		} else {
			component := text(at(resource, "metadata", "labels", "app.kubernetes.io/name"))
			switch component {
			case "node-exporter", "metrics-server", "blackbox-exporter":
				use = "`" + component + ".enabled=true`"
			}
		}
		pod := object(at(resource, "spec", "template", "spec"))
		if kind == "Pod" || kind == "OpenTelemetryCollector" {
			pod = object(resource["spec"])
		}
		if kind == "CronJob" {
			pod = object(at(resource, "spec", "jobTemplate", "spec", "template", "spec"))
		}
		if kind == "OpenTelemetryCollector" {
			collectors = append(collectors, resource)
			use = "Collector sidecar"
		}
		isOperator := kind == "Deployment" && text(at(resource, "metadata", "labels", "app.kubernetes.io/name")) == "opentelemetry-operator"
		if isOperator {
			use = "Operator"
		}
		for _, field := range []string{"containers", "initContainers", "ephemeralContainers", "additionalContainers"} {
			containers, _ := pod[field].([]any)
			for _, item := range containers {
				container := object(item)
				if image := text(container["image"]); image != "" {
					images.add(image, use)
				}
				if isOperator {
					args, _ := container["args"].([]any)
					for index, arg := range args {
						flag, image, joined := strings.Cut(text(arg), "=")
						if flag != "--collector-image" && flag != "--target-allocator-image" {
							continue
						}
						if !joined && index+1 < len(args) {
							image = text(args[index+1])
						}
						if previous := defaults[flag]; previous != "" && previous != image {
							return nil, fmt.Errorf("conflicting Operator defaults for %s", flag)
						}
						defaults[flag] = image
					}
				}
			}
		}
		if kind == "OpAMPBridge" || kind == "TargetAllocator" {
			if image := text(at(resource, "spec", "image")); image != "" {
				images.add(image, kind)
			}
		}
	}
	for _, collector := range collectors {
		name := text(at(collector, "metadata", "name"))
		image := text(at(collector, "spec", "image"))
		if image == "" {
			image = defaults["--collector-image"]
		}
		if image == "" {
			return nil, fmt.Errorf("cannot resolve Collector image for %s; configure an explicit image when using an external Operator", name)
		}
		images.add(image, "Collector")
		if enabled, _ := at(collector, "spec", "targetAllocator", "enabled").(bool); enabled {
			image = text(at(collector, "spec", "targetAllocator", "image"))
			if image == "" {
				image = defaults["--target-allocator-image"]
			}
			if image == "" {
				return nil, fmt.Errorf("cannot resolve Target Allocator image for %s; configure an explicit image when using an external Operator", name)
			}
			images.add(image, "Target Allocator")
		}
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("rendered chart contains no images")
	}
	if err := images.validate(); err != nil {
		return nil, err
	}
	return images, nil
}

func render(ctx context.Context, root string, optional bool) (imageSet, error) {
	args := []string{"template", "nktp", filepath.Join(root, "nktp")}
	if optional {
		args = append(args, "--set", "metrics-server.enabled=true", "--set", "blackbox-exporter.enabled=true")
	}
	command := exec.CommandContext(ctx, "helm", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	data, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("helm template: %w\n%s", err, stderr.String())
	}
	return discover(data)
}

// ResolvedImages holds the rendered inventory and Operator images used to
// resolve documentation references during release preparation.
type ResolvedImages struct {
	Table          string
	operatorImages []string
}

func (images ResolvedImages) operatorVersion() (string, error) {
	version := ""
	for _, image := range images.operatorImages {
		name, _, _ := strings.Cut(image, "@")
		separator := strings.LastIndex(name, ":")
		if separator <= strings.LastIndex(name, "/") {
			return "", fmt.Errorf("cannot resolve Operator documentation version from image %q: a semantic version tag is required", image)
		}
		tag := strings.TrimPrefix(name[separator+1:], "v")
		if !versionPattern.MatchString(tag) {
			return "", fmt.Errorf("Operator image %q has no semantic version tag", image)
		}
		if version != "" && version != tag {
			return "", fmt.Errorf("conflicting Operator image versions: %s and %s", version, tag)
		}
		version = tag
	}
	if version == "" {
		return "", fmt.Errorf("cannot resolve Operator documentation version: no Operator image found")
	}
	return version, nil
}

// Inventory resolves chart defaults and optional NKTP components before any
// documentation is updated. Its inputs are rendered manifests, not pinning helpers.
func Inventory(ctx context.Context, root string) (ResolvedImages, error) {
	images, err := render(ctx, root, false)
	if err != nil {
		return ResolvedImages{}, err
	}
	optional, err := render(ctx, root, true)
	if err != nil {
		return ResolvedImages{}, err
	}
	for image, uses := range optional {
		if images[image] == nil {
			images[image] = uses
		}
	}
	resolved := ResolvedImages{Table: markdown(images)}
	for image, uses := range images {
		if uses["Operator"] {
			resolved.operatorImages = append(resolved.operatorImages, image)
		}
	}
	return resolved, nil
}

func markdown(images imageSet) string {
	var references []string
	for image := range images {
		references = append(references, image)
	}
	slices.Sort(references)
	var output strings.Builder
	output.WriteString("| Full image reference | Use |\n| --- | --- |\n")
	for _, image := range references {
		var uses []string
		for use := range images[image] {
			uses = append(uses, use)
		}
		slices.Sort(uses)
		fmt.Fprintf(&output, "| `%s` | %s |\n", image, strings.Join(uses, "; "))
	}
	return output.String()
}
