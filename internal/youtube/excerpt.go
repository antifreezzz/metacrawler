package youtube

// ExcerptStrategy задает способ отбора фрагмента транскрипта для отправки в LLM.
type ExcerptStrategy string

const (
	ExcerptHead        ExcerptStrategy = "head"
	ExcerptTail        ExcerptStrategy = "tail"
	ExcerptHeadTail    ExcerptStrategy = "head+tail"
	ExcerptMultiWindow ExcerptStrategy = "multi-window"
	ExcerptFull        ExcerptStrategy = "full"
)

const excerptSeparator = " … "

// SelectTranscriptExcerpt возвращает фрагмент транскрипта длиной не более budget рун
// согласно выбранной стратегии. Отбор ведется по рунам, а не по байтам, чтобы не
// разрезать многобайтовые символы (кириллица). budget <= 0 или budget >= длины
// транскрипта возвращает текст целиком.
func SelectTranscriptExcerpt(transcript string, strategy ExcerptStrategy, budget int) string {
	runes := []rune(transcript)
	n := len(runes)
	if budget <= 0 || budget >= n {
		return transcript
	}

	switch strategy {
	case ExcerptTail:
		return string(runes[n-budget:])

	case ExcerptHeadTail:
		half := budget / 2
		head := runes[:half]
		tail := runes[n-(budget-half):]
		return string(head) + excerptSeparator + string(tail)

	case ExcerptMultiWindow:
		window := budget / 3
		if window == 0 {
			return string(runes[:budget])
		}
		start := runes[:window]

		midStart := n/2 - window/2
		if midStart < window {
			midStart = window
		}
		if midStart+window > n-window {
			midStart = n - 2*window
		}
		middle := runes[midStart : midStart+window]

		end := runes[n-window:]
		return string(start) + excerptSeparator + string(middle) + excerptSeparator + string(end)

	case ExcerptFull:
		return transcript

	default:
		return string(runes[:budget])
	}
}
