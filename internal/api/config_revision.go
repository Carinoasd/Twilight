package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prejudice-studio/twilight/internal/config"
)

type configEditSnapshot struct {
	content   string
	revision  string
	file      config.Config
	effective config.Config
}

// Revision covers both files and effective environment overrides, but never
// exposes their raw bytes. Reads also detect changes during the snapshot.
func (a *App) configEditSnapshot() (configEditSnapshot, error) {
	path := a.configFilePath()
	read := func() ([]byte, error) {
		var parts [][]byte
		local := os.Getenv("TWILIGHT_CONFIG_LOCAL_FILE")
		if local == "" {
			local = strings.TrimSuffix(path, filepath.Ext(path)) + ".local" + filepath.Ext(path)
		}
		for _, name := range []string{path, local} {
			data, err := os.ReadFile(name)
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			parts = append(parts, data)
		}
		return json.Marshal(parts)
	}
	before, err := read()
	if err != nil {
		return configEditSnapshot{}, err
	}
	file, err := config.LoadFileOnly(path)
	if err != nil {
		return configEditSnapshot{}, err
	}
	effective, err := config.Load(path)
	if err != nil {
		return configEditSnapshot{}, err
	}
	after, err := read()
	if err != nil {
		return configEditSnapshot{}, err
	}
	if string(before) != string(after) {
		return configEditSnapshot{}, errors.New("configuration changed while reading")
	}
	var parts [][]byte
	if err = json.Unmarshal(after, &parts); err != nil {
		return configEditSnapshot{}, err
	}
	effectiveBytes, err := json.Marshal(effective)
	if err != nil {
		return configEditSnapshot{}, err
	}
	hash := sha256.New()
	_, _ = hash.Write(after)
	_, _ = hash.Write(effectiveBytes)
	return configEditSnapshot{content: string(parts[0]), revision: hex.EncodeToString(hash.Sum(nil)), file: file, effective: effective}, nil
}

// Lock order is config file lock -> runtimeMu. All normal saves/restore and
// internal partial editors use this boundary before reading their base values.
func (a *App) editConfig(expected string, edit func(configEditSnapshot) (string, error)) (map[string]any, int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var info map[string]any
	status := http.StatusInternalServerError
	message := "配置保存失败"
	err := config.WithWriteLock(ctx, a.configFilePath(), func() error {
		a.runtimeMu.Lock()
		defer a.runtimeMu.Unlock()
		snapshot, err := a.configEditSnapshot()
		if err != nil {
			message = "配置读取失败，请重新加载"
			return nil
		}
		if expected != "" && expected != snapshot.revision {
			status = http.StatusConflict
			message = "配置已被修改，请重新加载后再保存"
			return nil
		}
		content, err := edit(snapshot)
		if err != nil {
			status = http.StatusBadRequest
			message = "配置内容无效"
			return nil
		}
		latest, err := a.configEditSnapshot()
		if err != nil || latest.revision != snapshot.revision {
			status = http.StatusConflict
			message = "配置已被修改，请重新加载后再保存"
			return nil
		}
		info, status, message = a.saveConfigContentLocked(content, snapshot.revision)
		if status == http.StatusOK {
			if next, err := a.configEditSnapshot(); err == nil {
				info["revision"] = next.revision
			}
		}
		return nil
	})
	if err != nil {
		return nil, http.StatusServiceUnavailable, "配置保存暂不可用，请稍后重试"
	}
	return info, status, message
}

func (a *App) patchConfigSections(expected string, sections map[string]any) (map[string]any, int, string) {
	return a.editConfig(expected, func(snapshot configEditSnapshot) (string, error) {
		values := configValues(snapshot.file)
		for _, section := range configSectionDefs() {
			fields, _ := sections[section.Key].(map[string]any)
			for _, field := range section.Fields {
				value, ok := fields[field.Key]
				if !ok {
					continue
				}
				if field.Type == "secret" && asString(value) == secretMaskValue {
					continue
				}
				values[section.Key][field.Key] = normalizeConfigField(field, value)
			}
		}
		ensureTicketDefaults(values)
		return mergeConfigTOML(snapshot.content, values)
	})
}

func failConfigEdit(w http.ResponseWriter, status int, message string) {
	if status == http.StatusConflict {
		failWithCode(w, status, ErrConfigRevisionConflict, message)
		return
	}
	failWithCode(w, status, ErrConfigSaveFailed, message)
}

func writeConfigCandidate(path string, data []byte) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

// Failed preparation never publishes a runtime. Restore only our own candidate;
// an external editor's newer bytes must not be overwritten by rollback.
func rollbackConfigCandidate(path, candidate string, original []byte, existed bool) error {
	current, err := os.ReadFile(path)
	if err != nil || string(current) != candidate {
		return errors.New("configuration changed before rollback")
	}
	if !existed {
		return os.Remove(path)
	}
	tmp := path + "." + time.Now().Format("20060102150405.000000000") + ".rollback.tmp"
	if err := writeConfigCandidate(tmp, original); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}
