package youtube

import "fmt"

// AudioWindow задает временной отрезок видео для сэмплирования аудио (в секундах).
type AudioWindow struct {
	Start int
	End   int
}

// SectionArg возвращает аргумент yt-dlp --download-sections для окна.
func (w AudioWindow) SectionArg() string {
	return fmt.Sprintf("*%s-%s", FormatTimestamp(w.Start), FormatTimestamp(w.End))
}

// FormatTimestamp переводит секунды в формат HH:MM:SS, который понимает yt-dlp.
func FormatTimestamp(sec int) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, (sec%3600)/60, sec%60)
}

// BuildAudioWindows выбирает до maxWindows непересекающихся окон аудио для Whisper:
// начало, середина и конец ролика. Это позволяет поймать вердикт блогера, который
// обычно звучит в конце, а не только вступление. При неизвестной длительности
// возвращает одно стартовое окно (прежнее поведение).
func BuildAudioWindows(windowSec, durationSec, maxWindows int) []AudioWindow {
	if windowSec <= 0 {
		windowSec = 60
	}
	if maxWindows <= 0 {
		maxWindows = 3
	}
	if maxWindows > 3 {
		maxWindows = 3
	}

	if durationSec <= 0 {
		return []AudioWindow{{Start: 0, End: windowSec}}
	}
	if durationSec <= 2*windowSec {
		return []AudioWindow{{Start: 0, End: durationSec}}
	}

	start := AudioWindow{Start: 0, End: windowSec}
	end := AudioWindow{Start: durationSec - windowSec, End: durationSec}

	switch maxWindows {
	case 1:
		return []AudioWindow{start}
	case 2:
		return []AudioWindow{start, end}
	default:
		midStart := durationSec/2 - windowSec/2
		middle := AudioWindow{Start: midStart, End: midStart + windowSec}
		return []AudioWindow{start, middle, end}
	}
}
