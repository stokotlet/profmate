package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/stokotlet/profmate/internal/model"
	"github.com/stokotlet/profmate/internal/render"
)

func Save(dir string, s *model.Session) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
	}{{"session.json", append(data, '\n')}, {"report.md", []byte(render.Markdown(s))}, {"chat.md", []byte(render.Chat(s))}}
	for _, f := range files {
		if err := atomicWrite(filepath.Join(dir, f.name), f.data); err != nil {
			return err
		}
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".profmate-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Load(dir string) (*model.Session, error) {
	f, err := os.Open(filepath.Join(dir, "session.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 32<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 32<<20 {
		return nil, errors.New("session.json слишком большой")
	}
	var s model.Session
	if err = json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("не удалось прочитать сессию: %w", err)
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("неподдерживаемая версия сессии: %d", s.Version)
	}
	return &s, nil
}
