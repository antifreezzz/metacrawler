package youtube_test

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/youtube"
)

func TestSelectTranscriptExcerpt_Head(t *testing.T) {
	text := "abcdefghijklmnopqrstuvwxyz"
	got := youtube.SelectTranscriptExcerpt(text, youtube.ExcerptHead, 10)
	require.Equal(t, "abcdefghij", got)
}

func TestSelectTranscriptExcerpt_Tail(t *testing.T) {
	text := "abcdefghijklmnopqrstuvwxyz"
	got := youtube.SelectTranscriptExcerpt(text, youtube.ExcerptTail, 10)
	require.Equal(t, "qrstuvwxyz", got)
}

func TestSelectTranscriptExcerpt_HeadTail(t *testing.T) {
	text := "abcdefghijklmnopqrstuvwxyz"
	got := youtube.SelectTranscriptExcerpt(text, youtube.ExcerptHeadTail, 10)
	require.Equal(t, "abcde … vwxyz", got)
}

func TestSelectTranscriptExcerpt_MultiWindow(t *testing.T) {
	text := "012345678901234567890123456789"
	got := youtube.SelectTranscriptExcerpt(text, youtube.ExcerptMultiWindow, 9)
	require.Equal(t, "012 … 456 … 789", got)
}

func TestSelectTranscriptExcerpt_Full(t *testing.T) {
	text := "abcdefghijklmnopqrstuvwxyz"
	got := youtube.SelectTranscriptExcerpt(text, youtube.ExcerptFull, 10)
	require.Equal(t, text, got)
}

func TestSelectTranscriptExcerpt_BudgetTooBigReturnsWhole(t *testing.T) {
	text := "short text"
	require.Equal(t, text, youtube.SelectTranscriptExcerpt(text, youtube.ExcerptHead, 1000))
	require.Equal(t, text, youtube.SelectTranscriptExcerpt(text, youtube.ExcerptTail, 1000))
	require.Equal(t, text, youtube.SelectTranscriptExcerpt(text, youtube.ExcerptHeadTail, 1000))
	require.Equal(t, text, youtube.SelectTranscriptExcerpt(text, youtube.ExcerptMultiWindow, 1000))
}

func TestSelectTranscriptExcerpt_ZeroBudgetReturnsWhole(t *testing.T) {
	text := "some transcript"
	require.Equal(t, text, youtube.SelectTranscriptExcerpt(text, youtube.ExcerptHead, 0))
	require.Equal(t, text, youtube.SelectTranscriptExcerpt(text, youtube.ExcerptTail, 0))
}

func TestSelectTranscriptExcerpt_RuneSafe(t *testing.T) {
	text := "абвгдеёжзийклмнопрстуфхцчшщъыьэюя"
	head := youtube.SelectTranscriptExcerpt(text, youtube.ExcerptHead, 10)
	require.True(t, utf8.ValidString(head))
	require.Equal(t, 10, utf8.RuneCountInString(head))
	require.Equal(t, "абвгдеёжзи", head)

	tail := youtube.SelectTranscriptExcerpt(text, youtube.ExcerptTail, 10)
	require.True(t, utf8.ValidString(tail))
	require.Equal(t, 10, utf8.RuneCountInString(tail))
}
