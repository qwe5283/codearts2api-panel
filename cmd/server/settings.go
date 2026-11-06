package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

type fileSettings struct {
	mu   sync.Mutex
	path string
	// apply 落盘成功后热应用配置并返回仍需重启的字段（由 main 注入）。
	// 为 nil 表示没有可热应用的组件（测试/裸用），全部按"无需重启"返回。
	apply func(*Config) []string
}

func configMap(c *Config) (map[string]any, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(raw, &result)
	return result, err
}

func (s *fileSettings) readLocked() (map[string]any, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(stripJSONComments(raw), &values); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	if values == nil {
		return nil, errors.New("configuration must be an object")
	}
	base, err := configMap(Default())
	if err != nil {
		return nil, err
	}
	mergeConfig(base, values)
	// 面板上显示的就是运行期会用的值：非法值按默认值回显，别让人以为 0 真的生效了。
	// 这里尽力而为——手工改坏的类型（如 err_threshold 写成字符串）留给 update 报错，
	// 读取阶段仍把原样内容交给面板，否则配置页会整页打不开。
	_, _ = normalizeSettings(base)
	return base, nil
}

func redactConfig(values map[string]any) map[string]any {
	values["api_key"] = ""
	values["admin_key"] = ""
	return values
}

func (s *fileSettings) Read() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	return redactConfig(values), nil
}

func mergeConfig(base, patch map[string]any) {
	for key, value := range patch {
		nested, isMap := value.(map[string]any)
		existing, hasMap := base[key].(map[string]any)
		if isMap && hasMap {
			mergeConfig(existing, nested)
		} else {
			base[key] = value
		}
	}
}

func (s *fileSettings) Update(patch map[string]any) (map[string]any, error) {
	values, _, err := s.update(patch)
	return values, err
}

// UpdateLive 保存配置并热应用，返回仍需重启的字段名（面板据此提示）。
func (s *fileSettings) UpdateLive(patch map[string]any) (map[string]any, []string, error) {
	return s.update(patch)
}

// update 校验 → 落盘 → 热应用。校验失败不写盘；热应用只在落盘成功后执行。
func (s *fileSettings) update(patch map[string]any) (map[string]any, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.readLocked()
	if err != nil {
		return nil, nil, err
	}
	// Empty redacted keys mean "keep"; null or a non-string is invalid.
	for _, key := range []string{"api_key", "admin_key"} {
		if value, present := patch[key]; present {
			text, ok := value.(string)
			if !ok {
				return nil, nil, fmt.Errorf("%s must be a string", key)
			}
			if text == "" {
				patch[key] = values[key]
			}
		}
	}
	mergeConfig(values, patch)
	// 归一化：非法值（0/负数/解析不了的时长）回写成默认值，并把结果写回待落盘的 map。
	// 这样「文件里写的」就是「运行时真正用的」——否则 queue_max_attempts: 0 在文件里
	// 看着像「不重试」，实际却按默认值跑，面板上的数字也就没法信任。
	changes, err := normalizeSettings(values)
	if err != nil {
		return nil, nil, err
	}
	for _, change := range changes {
		log.Printf("config: %s 不是合法取值，已回写默认值", change)
	}
	raw, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	candidate := Default()
	if err := json.Unmarshal(raw, candidate); err != nil {
		return nil, nil, fmt.Errorf("invalid configuration: %w", err)
	}
	applyEnv(candidate)
	if err := candidate.validate(); err != nil {
		return nil, nil, err
	}
	if err := writeConfigAtomic(s.path, append(raw, '\n')); err != nil {
		return nil, nil, err
	}
	var restart []string
	if s.apply != nil {
		restart = s.apply(candidate)
	}
	// 返回落盘后的那份配置（数字统一成 JSON 解码后的形态），面板据此回填表单，
	// 用户立刻能看到 0 被改成了 10/30，而不是存完还是 0。
	written := map[string]any{}
	if err := json.Unmarshal(raw, &written); err != nil {
		return nil, nil, err
	}
	return redactConfig(written), restart, nil
}

// normalizeSettings 按 Config.normalize 的口径修正待读写/待落盘的配置 map，返回被
// 改写的字段说明。只回写重试、冷却与多模态策略的归一化字段，扩展键原样保留。
func normalizeSettings(values map[string]any) ([]string, error) {
	candidate := Default()
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, candidate); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	changes := candidate.normalize()
	setConfigValue(values, "cooldown.soft_rate", candidate.Cooldown.SoftRate)
	setConfigValue(values, "cooldown.err_threshold", candidate.Cooldown.ErrThresh)
	setConfigValue(values, "cooldown.err_cooldown", candidate.Cooldown.ErrCooldown)
	setConfigValue(values, "account_strategy", candidate.AccountStrategy)
	setConfigValue(values, "queue_retry_seconds", candidate.QueueRetrySeconds)
	setConfigValue(values, "queue_max_attempts", candidate.QueueMaxAttempts)
	setConfigValue(values, "session_ttl", candidate.SessionTTL)
	setConfigValue(values, "reserve_credits", candidate.ReserveCredits)
	setConfigValue(values, "max_body_mb", candidate.MaxBodyMB)
	setConfigValue(values, "image_shrink.trigger_body_mb", candidate.ImageShrink.TriggerBodyMB)
	setConfigValue(values, "image_shrink.target_image_mb", candidate.ImageShrink.TargetImageMB)
	setConfigValue(values, "image_shrink.target_total_mb", candidate.ImageShrink.TargetTotalMB)
	setConfigValue(values, "image_shrink.max_dimension", candidate.ImageShrink.MaxDimension)
	setConfigValue(values, "image_shrink.quality", candidate.ImageShrink.Quality)
	setConfigValue(values, "vision_bridge.model", candidate.VisionBridge.Model)
	setConfigValue(values, "vision_bridge.max_images", candidate.VisionBridge.MaxImages)
	setConfigValue(values, "vision_bridge.max_chars", candidate.VisionBridge.MaxChars)
	setConfigValue(values, "vision_bridge.max_tokens", candidate.VisionBridge.MaxTokens)
	setConfigValue(values, "vision_bridge.timeout_seconds", candidate.VisionBridge.TimeoutSeconds)
	setConfigValue(values, "vision_bridge.total_timeout_seconds", candidate.VisionBridge.TotalSeconds)
	return changes, nil
}

// setConfigValue 按 "a.b" 路径写回一个值，父级不是对象时补一个。
// 只用来把归一化结果写进待落盘的 map，其余键（含面板不认识的扩展字段）原样保留。
func setConfigValue(values map[string]any, path string, value any) {
	keys := strings.Split(path, ".")
	target := values
	for _, key := range keys[:len(keys)-1] {
		next, ok := target[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			target[key] = next
		}
		target = next
	}
	target[keys[len(keys)-1]] = value
}

func writeConfigAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("configuration path must be a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		// 单文件 Docker bind mount 的挂载点无法被 rename 覆盖（Linux 返回 EBUSY）。
		// 普通文件继续走原子替换，这种部署形态则退回原地截断写入。
		if !errors.Is(err, syscall.EBUSY) {
			return err
		}
		log.Printf("config: %s 是 bind mount 挂载点，无法原子替换，改为原地写入", path)
		out, readErr := os.ReadFile(file.Name())
		if readErr != nil {
			return readErr
		}
		target, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			return fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := target.Write(out)
		if writeErr == nil {
			writeErr = target.Sync()
		}
		closeErr := target.Close()
		// 写失败时保留 tmp：挂载文件已被 O_TRUNC 清空，tmp 里是完整新内容，可手工恢复。
		if writeErr != nil {
			return fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", file.Name(), writeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	}
	return nil
}

func ensureConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	c := Default()
	newKey := func() (string, error) {
		var key [32]byte
		if _, err := rand.Read(key[:]); err != nil {
			return "", err
		}
		return hex.EncodeToString(key[:]), nil
	}
	var err error
	if c.APIKey, err = newKey(); err != nil {
		return err
	}
	if c.AdminKey, err = newKey(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}
