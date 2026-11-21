package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var remindAfter = regexp.MustCompile("^(?:请)?([0-9一二两三四五六七八九十]{1,4})\\s*(秒钟?|分钟|分|小时)后[，,\\s]*提醒我\\s*(.+)$")
var timerFor = regexp.MustCompile("^(?:请)?(?:帮我)?(?:(?:设置?|开始)(?:一个|个)?)?(?:倒)?计时[：:，,\\s]*([0-9一二两三四五六七八九十]{1,4})\\s*(秒钟?|分钟|分|小时)$")

func voiceNumber(text string) int {
	if n, err := strconv.Atoi(text); err == nil {
		return n
	}
	digits := map[string]int{"一": 1, "二": 2, "两": 2, "三": 3, "四": 4, "五": 5, "六": 6, "七": 7, "八": 8, "九": 9}
	if n, ok := digits[text]; ok {
		return n
	}
	before, after, ok := strings.Cut(text, "十")
	if !ok {
		return 0
	}
	tens := digits[before]
	if before == "" {
		tens = 1
	}
	if tens == 0 || (after != "" && digits[after] == 0) {
		return 0
	}
	return tens*10 + digits[after]
}

var quickTimer = Manifest{
	ID: "quick-timer", Version: "0.1.0", Name: "语音计时", Description: "到时播报一次。",
	Fields: []Field{{Key: "content", Label: "播报内容", Kind: "text", Required: true}},
	Steps:  []Step{{Kind: "say", Text: "{{content}}"}},
}

type voiceTaskReply struct {
	text    string
	handled bool
	timer   bool
}

func (p *Platform) voiceTask(text string, now time.Time) (voiceTaskReply, error) {
	text = strings.TrimRight(strings.TrimSpace(text), "。！？.!? ")
	if text == "继续下一步" || text == "完成当前任务" {
		reply := "继续。"
		err := p.change(func(s *State) error {
			var id string
			for _, task := range s.Tasks {
				if task.Status != "waiting" {
					continue
				}
				if id != "" {
					reply = "有多条任务等待确认，请在控制台选择。"
					return errNoVoiceChange
				}
				id = task.ID
			}
			if id == "" {
				reply = "目前没有等待确认的任务。"
				return errNoVoiceChange
			}
			return taskAction(s, id, "confirm", now)
		})
		if errors.Is(err, errNoVoiceChange) {
			err = nil
		}
		return voiceTaskReply{text: reply, handled: true}, err
	}
	match := remindAfter.FindStringSubmatch(text)
	ability := "reminder"
	if match == nil {
		match = timerFor.FindStringSubmatch(text)
		ability = quickTimer.ID
	}
	if match == nil {
		return voiceTaskReply{}, nil
	}
	n := voiceNumber(match[1])
	unit := time.Second
	switch match[2] {
	case "分钟", "分":
		unit = time.Minute
	case "小时":
		unit = time.Hour
	}
	if n < 1 || n > 86400/int(unit/time.Second) {
		return voiceTaskReply{text: "请设置一天以内的提醒或计时。", handled: true}, nil
	}
	duration := time.Duration(n) * unit
	content := "计时结束。"
	if ability == "reminder" {
		content = strings.TrimSpace(match[3])
	}
	if content == "" || len(content) > 2000 {
		return voiceTaskReply{text: "请简短说出要提醒的事情。", handled: true}, nil
	}
	if err := p.scheduleVoiceTask(ability, content, duration, now); err != nil {
		return voiceTaskReply{handled: true}, err
	}
	return voiceTaskReply{text: fmt.Sprintf("好的，已设定%s%s。", match[1], match[2]), handled: true, timer: ability == quickTimer.ID}, nil
}

func (p *Platform) scheduleVoiceTask(ability, content string, duration time.Duration, now time.Time) error {
	return p.change(func(s *State) error {
		if ability == quickTimer.ID {
			kept := s.Schedules[:0]
			for _, schedule := range s.Schedules {
				if schedule.AbilityID != quickTimer.ID || schedule.Enabled {
					kept = append(kept, schedule)
				}
			}
			s.Schedules = kept
		}
		if len(s.Schedules) >= 100 {
			return errors.New("定时计划已满")
		}
		sc := Schedule{ID: newID(), AbilityID: ability, Inputs: map[string]string{"content": content},
			NextAt: now.Add(duration), Repeat: "once", Timezone: s.Settings.Timezone, Enabled: true}
		if _, ok := s.Abilities[ability]; !ok && ability == quickTimer.ID {
			if len(s.Abilities) >= 32 {
				return errors.New("能力列表已满")
			}
			s.Abilities[ability] = Ability{Manifest: quickTimer, Enabled: true, Config: map[string]string{}}
		}
		if err := validateSchedule(s, sc, now); err != nil {
			return err
		}
		s.Schedules = append(s.Schedules, sc)
		return nil
	})
}

var errNoVoiceChange = errors.New("没有要保存的语音任务变更")
