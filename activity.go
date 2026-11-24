package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const usageBytes = 1 << 20
const dialogueBytes = 2 << 20
const usagePageSize = 50

type usageBucket struct {
	Day          string  `json:"day"`
	Service      string  `json:"service"`
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Currency     string  `json:"currency,omitempty"`
	Calls        int64   `json:"calls"`
	AudioMS      int64   `json:"audio_ms"`
	Characters   int64   `json:"characters"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Metered      int64   `json:"metered"`
	KnownCost    int64   `json:"known_cost"`
	Estimated    float64 `json:"estimated"`
}

type usageRate struct {
	Service       string    `json:"service"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	Currency      string    `json:"currency"`
	PerMillionIn  *float64  `json:"per_million_in"`
	PerMillionOut *float64  `json:"per_million_out"`
	PerMillionHit *float64  `json:"per_million_hit"`
	PerAudioHour  *float64  `json:"per_audio_hour"`
	PerRequest    *float64  `json:"per_request,omitempty"`
	Per10kChars   *float64  `json:"per_10k_chars"`
	UpdatedAt     time.Time `json:"updated_at"`
	Source        string    `json:"source"`
}

type dialogueRecord struct {
	ID         string      `json:"id"`
	At         time.Time   `json:"at"`
	Transcript string      `json:"transcript"`
	Reply      string      `json:"reply"`
	Timing     voiceResult `json:"timing"`
}

type activityData struct {
	LocalLast map[string]UsageRecord `json:"last_local_service,omitempty"`
	Imported  bool                   `json:"legacy_imported"`
	Recent    []UsageRecord          `json:"recent"`
	Daily     map[string]usageBucket `json:"daily"`
	Rates     map[string]usageRate   `json:"rates"`
	Logging   bool                   `json:"dialogue_logging"`
	Dialogues []dialogueRecord       `json:"dialogues"`
}

// Local service results are diagnostic evidence, separate from billable usage.
func (a *activityStore) observeLocal(service, provider string, revision int, err error, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loadErr != nil {
		return
	}
	if a.data.LocalLast == nil {
		a.data.LocalLast = map[string]UsageRecord{}
	}
	if a.data.LocalLast[service].At.After(at) {
		return
	}
	a.data.LocalLast[service] = UsageRecord{Service: service, Provider: provider, ConfigRevision: revision, At: at, Success: err == nil}
	a.dirty = true
}

func (a *activityStore) localResults() []UsageRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows := make([]UsageRecord, 0, len(a.data.LocalLast))
	for _, entry := range a.data.LocalLast {
		rows = append(rows, entry)
	}
	return rows
}

type activityStore struct {
	mu      sync.Mutex
	path    string
	data    activityData
	dirty   bool
	loadErr error
}

func openActivity(path string) *activityStore {
	a := &activityStore{path: path}
	body, err := readLimitedFile(path)
	if err == nil {
		a.loadErr = json.Unmarshal(body, &a.data)
	} else if !os.IsNotExist(err) {
		a.loadErr = err
	}
	if a.data.Daily == nil {
		a.data.Daily = map[string]usageBucket{}
	}
	if a.data.Rates == nil {
		a.data.Rates = map[string]usageRate{}
	}
	return a
}

func usageKey(parts ...string) string { return strings.Join(parts, "\x00") }

func (r usageRate) estimate(u UsageRecord) (float64, bool) {
	if !u.Success && (u.Service == "asr" || u.Service == "tts") {
		return 0, false
	}
	cost := func(amount *int64, price *float64, divisor float64) (float64, bool) {
		if amount == nil || price == nil || *amount < 0 {
			return 0, false
		}
		return float64(*amount) * *price / divisor, true
	}
	switch u.Service {
	case "search":
		if u.BillableRequest == nil || r.PerRequest == nil {
			return 0, false
		}
		if !*u.BillableRequest {
			return 0, true
		}
		return *r.PerRequest, true
	case "asr":
		return cost(u.AudioMS, r.PerAudioHour, 3_600_000)
	case "tts":
		return cost(u.Characters, r.Per10kChars, 10_000)
	default:
		if u.InputTokens == nil || u.OutputTokens == nil || (u.CacheWriteTokens != nil && *u.CacheWriteTokens != 0) {
			return 0, false
		}
		input := *u.InputTokens
		var hitCost float64
		if u.CachedInputTokens != nil {
			hit := *u.CachedInputTokens
			if hit < 0 || hit > input {
				return 0, false
			}
			if hit != 0 {
				value, ok := cost(&hit, r.PerMillionHit, 1_000_000)
				if !ok {
					return 0, false
				}
				hitCost = value
			}
			input -= hit
		} else if r.PerMillionHit != nil {
			return 0, false
		}
		in, inOK := cost(&input, r.PerMillionIn, 1_000_000)
		out, outOK := cost(u.OutputTokens, r.PerMillionOut, 1_000_000)
		return in + out + hitCost, inOK && outOK
	}
}

func (a *activityStore) add(u UsageRecord, timezone string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addLocked(u, timezone)
}

func (a *activityStore) addLocked(u UsageRecord, timezone string) {
	if a.loadErr != nil {
		return
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	u.Day = u.At.In(loc).Format(time.DateOnly)
	rate, found := a.data.Rates[usageKey(u.Service, u.Provider, u.Model)]
	if !found {
		rate, found = builtinUsageRate(u)
	}
	if found {
		if cost, ok := rate.estimate(u); ok {
			u.Estimated = &cost
			u.Currency = rate.Currency
			u.PriceAt = &rate.UpdatedAt
			u.PriceSource = rate.Source
		}
	}
	key := usageKey(u.Day, u.Service, u.Provider, u.Model, u.Currency)
	b := a.data.Daily[key]
	b.Day, b.Service, b.Provider, b.Model, b.Currency = u.Day, u.Service, u.Provider, u.Model, u.Currency
	b.Calls++
	if u.AudioMS != nil || u.Characters != nil || u.InputTokens != nil || u.OutputTokens != nil || u.BillableRequest != nil {
		b.Metered++
	}
	if u.AudioMS != nil {
		b.AudioMS += *u.AudioMS
	}
	if u.Characters != nil {
		b.Characters += *u.Characters
	}
	if u.InputTokens != nil {
		b.InputTokens += *u.InputTokens
	}
	if u.OutputTokens != nil {
		b.OutputTokens += *u.OutputTokens
	}
	if u.Estimated != nil {
		b.KnownCost++
		b.Estimated += *u.Estimated
	}
	a.data.Daily[key] = b
	a.data.Recent = append(a.data.Recent, u)
	a.dirty = true
	if len(a.data.Recent) > 1_000 {
		a.data.Recent = a.data.Recent[len(a.data.Recent)-1_000:]
	}
}

func builtinUsageRate(u UsageRecord) (usageRate, bool) {
	rate := usageRate{Service: u.Service, Provider: u.Provider, Model: u.Model, Currency: "CNY",
		Source: "火山引擎公开后付费单价", UpdatedAt: time.Date(2026, time.November, 21, 0, 0, 0, 0, time.UTC)}
	switch {
	case u.Service == "search" && u.Provider == "deepseek":
		price := 0.005
		rate.PerRequest = &price
		rate.Source = "DeepSeek Search / Fetch 官方公开单价"
		rate.UpdatedAt = time.Date(2026, time.November, 24, 0, 0, 0, 0, time.UTC)
	case u.Service == "asr" && u.Provider == "doubao" && u.Model == doubaoASRResource:
		price := 1.0
		rate.PerAudioHour = &price
	case u.Service == "tts" && u.Provider == "deepseek":
		price := 3.0
		rate.Per10kChars = &price
		rate.Source = "DeepSeek 语音合成官方刊例价（未计限时折扣）"
		rate.UpdatedAt = time.Date(2026, time.November, 24, 0, 0, 0, 0, time.UTC)
	case u.Service == "tts" && u.Provider == "doubao":
		price := 3.0
		rate.Per10kChars = &price
	default:
		return usageRate{}, false
	}
	return rate, true
}

func retainActivity[T any](list []T, deadline time.Time, maxBytes int, timestamp func(T) time.Time) []T {
	next := list[:0]
	for _, row := range list {
		if !timestamp(row).Before(deadline) {
			next = append(next, row)
		}
	}
	size := 2
	first := len(next)
	for first > 0 {
		body, _ := json.Marshal(next[first-1])
		size += len(body) + 1
		if size > maxBytes {
			break
		}
		first--
	}
	if first > 0 {
		next = append([]T{}, next[first:]...)
	}
	return next
}

func (a *activityStore) prune(now time.Time) {
	cutoff := now.AddDate(0, 0, -7)
	recentCount, dialogueCount, dayCount := len(a.data.Recent), len(a.data.Dialogues), len(a.data.Daily)
	a.data.Recent = retainActivity(a.data.Recent, cutoff, usageBytes, func(u UsageRecord) time.Time { return u.At })
	a.data.Dialogues = retainActivity(a.data.Dialogues, cutoff, dialogueBytes, func(d dialogueRecord) time.Time { return d.At })
	oldest := now.AddDate(0, 0, -89).Format(time.DateOnly)
	keys := make([]string, 0, len(a.data.Daily))
	for key, b := range a.data.Daily {
		if b.Day < oldest {
			delete(a.data.Daily, key)
		} else {
			keys = append(keys, key)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	size := 2
	for _, key := range keys {
		encoded, _ := json.Marshal(a.data.Daily[key])
		size += len(encoded) + len(key)*6 + 4
		if size > usageBytes {
			delete(a.data.Daily, key)
		}
	}
	if recentCount != len(a.data.Recent) || dialogueCount != len(a.data.Dialogues) || dayCount != len(a.data.Daily) {
		a.dirty = true
	}
}

func (a *activityStore) flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.flushLocked()
}

func (a *activityStore) flushLocked() error {
	if a.loadErr != nil {
		return a.loadErr
	}
	a.prune(time.Now())
	if !a.dirty {
		return nil
	}
	data, err := json.Marshal(a.data)
	if err != nil {
		return err
	}
	if err = writePrivateState(a.path, data); err == nil {
		a.dirty = false
	}
	return err
}

func (a *activityStore) importLegacy(rows []UsageRecord, timezone string) {
	if len(rows) == 0 || a.data.Imported || a.loadErr != nil {
		return
	}
	for _, row := range rows {
		a.addLocked(row, timezone)
	}
	a.data.Imported = true
	if err := a.flush(); err != nil {
		log.Print("旧用量记录暂未迁移")
	}
}
func (a *activityStore) recent() []UsageRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]UsageRecord(nil), a.data.Recent...)
}

func clipDialogue(text string) string {
	if len(text) <= 2048 {
		return text
	}
	end := 2048
	for !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func (a *activityStore) dialogue(text, reply string, timing voiceResult) {
	if sensitiveDialogue.MatchString(text) || sensitiveDialogue.MatchString(reply) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.data.Logging || a.loadErr != nil {
		return
	}
	a.data.Dialogues = append(a.data.Dialogues, dialogueRecord{newID(), time.Now().UTC(), clipDialogue(text), clipDialogue(reply), timing})
	if len(a.data.Dialogues) > 1_000 {
		a.data.Dialogues = a.data.Dialogues[len(a.data.Dialogues)-1_000:]
	}
	a.dirty = true
}

var activityCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
var sensitiveDialogue = regexp.MustCompile(`(?i)确认登录|登录口令|验证码|密码|访问令牌|api[ _-]*key|bearer[[:space:]]|sk-[a-z0-9_-]{12,}|-----BEGIN [A-Z ]*PRIVATE KEY-----`)

func (store *activityStore) usageSummary(timezone string) map[string]any {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	today := time.Now().In(loc).Format(time.DateOnly)
	month := today[:7]
	var calls, missing int64
	totals := map[string]float64{}
	store.mu.Lock()
	if store.loadErr != nil {
		store.mu.Unlock()
		return nil
	}
	for _, row := range store.data.Daily {
		if row.Day == today {
			calls += row.Calls
		}
		if row.Day <= today && strings.HasPrefix(row.Day, month) {
			missing += row.Calls - row.KnownCost
			if row.KnownCost > 0 {
				totals[row.Currency] += row.Estimated
			}
		}
	}
	store.mu.Unlock()
	return map[string]any{"today_calls": calls, "month_unpriced": missing, "month_costs": totals}
}

func (a *api) activityRoutes(m *http.ServeMux) {
	store := a.platform.activity
	m.HandleFunc("GET /api/usage/summary", func(w http.ResponseWriter, r *http.Request) {
		summary := store.usageSummary(a.platform.snapshot().Settings.Timezone)
		if summary == nil {
			writeJSON(w, 503, map[string]string{"error": "用量记录暂不可读"})
			return
		}
		writeJSON(w, 200, summary)
	})
	m.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		days, offset := activityPage(r)
		q := r.URL.Query()
		filter := func(service, provider string) bool {
			return (q.Get("type") == "" || q.Get("type") == service) && (q.Get("provider") == "" || q.Get("provider") == provider)
		}
		loc, err := time.LoadLocation(a.platform.snapshot().Settings.Timezone)
		if err != nil {
			loc = time.UTC
		}
		now := time.Now().In(loc)
		from := now.AddDate(0, 0, 1-days).Format(time.DateOnly)
		store.mu.Lock()
		if store.loadErr != nil {
			store.mu.Unlock()
			writeJSON(w, 503, map[string]string{"error": "用量记录暂不可读"})
			return
		}
		store.prune(now)
		daily := []usageBucket{}
		providers := map[string]bool{}
		for _, row := range store.data.Daily {
			providers[row.Provider] = true
			if row.Day >= from && filter(row.Service, row.Provider) {
				daily = append(daily, row)
			}
		}
		sort.Slice(daily, func(i, j int) bool { return daily[i].Day < daily[j].Day })
		rows := []UsageRecord{}
		total := 0
		for i := len(store.data.Recent) - 1; i >= 0; i-- {
			row := store.data.Recent[i]
			if row.Day < from || !filter(row.Service, row.Provider) {
				continue
			}
			if total >= offset && len(rows) < usagePageSize {
				rows = append(rows, row)
			}
			total++
		}
		rates := []usageRate{}
		for _, rate := range store.data.Rates {
			rates = append(rates, rate)
		}
		store.mu.Unlock()
		writeJSON(w, 200, map[string]any{"daily": daily, "recent": rows, "recent_count": total, "providers": providers, "rates": rates,
			"start_day": from, "end_day": now.Format(time.DateOnly)})
	})
	m.HandleFunc("PUT /api/usage/rates", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var in usageRate
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		in.Provider, in.Model = strings.TrimSpace(in.Provider), strings.TrimSpace(in.Model)
		in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
		validType := in.Service == "llm" || in.Service == "asr" || in.Service == "tts" || in.Service == "external" || in.Service == "ability"
		if !validType || len(in.Provider) == 0 || len(in.Provider) > 100 || len(in.Model) > 200 || !activityCurrency.MatchString(in.Currency) || strings.ContainsRune(in.Provider+in.Model, 0) {
			bad(w, errors.New("请检查计价服务、模型和三位币种代码"))
			return
		}
		for _, value := range []*float64{in.PerMillionIn, in.PerMillionOut, in.PerMillionHit, in.PerAudioHour, in.Per10kChars} {
			if value != nil && (*value < 0 || *value > 1_000_000) {
				bad(w, errors.New("单价须为非负数"))
				return
			}
		}
		in.UpdatedAt = time.Now().UTC()
		in.Source = "用户配置"
		key := usageKey(in.Service, in.Provider, in.Model)
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, exists := store.data.Rates[key]; !exists && len(store.data.Rates) >= 128 {
			bad(w, errors.New("最多保存 128 条单价"))
			return
		}
		err := store.persistChange(func() { store.data.Rates[key] = in })
		if err != nil {
			bad(w, errors.New("无法保存用量单价"))
			return
		}
		writeJSON(w, 200, in)
	})
	m.HandleFunc("GET /api/dialogues", func(w http.ResponseWriter, r *http.Request) {
		_, offset := activityPage(r)
		day := r.URL.Query().Get("date")
		loc, err := time.LoadLocation(a.platform.snapshot().Settings.Timezone)
		if err != nil {
			loc = time.UTC
		}
		store.mu.Lock()
		if store.loadErr != nil {
			store.mu.Unlock()
			writeJSON(w, 503, map[string]string{"error": "对话记录暂不可读"})
			return
		}
		store.prune(time.Now())
		rows := []dialogueRecord{}
		total := 0
		for i := len(store.data.Dialogues) - 1; i >= 0; i-- {
			row := store.data.Dialogues[i]
			if day != "" && row.At.In(loc).Format(time.DateOnly) != day {
				continue
			}
			if total >= offset && len(rows) < usagePageSize {
				rows = append(rows, row)
			}
			total++
		}
		enabled := store.data.Logging
		store.mu.Unlock()
		writeJSON(w, 200, map[string]any{"enabled": enabled, "items": rows, "count": total})
	})
	m.HandleFunc("PUT /api/dialogues", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if err := store.persistChange(func() { store.data.Logging = in.Enabled }); err != nil {
			bad(w, errors.New("无法保存对话记录设置"))
			return
		}
		writeJSON(w, 200, in)
	})
	m.HandleFunc("DELETE /api/dialogues", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		store.mu.Lock()
		defer store.mu.Unlock()
		if err := store.persistChange(func() {
			next := []dialogueRecord{}
			for _, row := range store.data.Dialogues {
				if id != "" && row.ID != id {
					next = append(next, row)
				}
			}
			store.data.Dialogues = next
		}); err != nil {
			bad(w, errors.New("无法清理对话记录"))
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
}

func (a *activityStore) persistChange(change func()) error {
	if a.loadErr != nil {
		return a.loadErr
	}
	before, err := json.Marshal(a.data)
	if err != nil {
		return err
	}
	wasDirty := a.dirty
	change()
	a.dirty = true
	if err = a.flushLocked(); err != nil {
		a.data = activityData{}
		_ = json.Unmarshal(before, &a.data)
		a.dirty = wasDirty
	}
	return err
}

func activityPage(r *http.Request) (days, offset int) {
	days, _ = strconv.Atoi(r.URL.Query().Get("days"))
	if days < 1 || days > 90 {
		days = 30
	}
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 || offset > 1000 {
		offset = 0
	}
	return days, offset
}

func writePrivateState(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func readLimitedFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 5<<20))
	if len(data) == 5<<20 {
		return nil, errors.New("记录文件超过大小上限")
	}
	return data, err
}
