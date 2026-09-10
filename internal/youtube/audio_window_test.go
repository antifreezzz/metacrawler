package youtube_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"metacrawler/internal/youtube"
)

func TestFormatTimestamp(t *testing.T) {
	require.Equal(t, "00:00:00", youtube.FormatTimestamp(0))
	require.Equal(t, "00:01:30", youtube.FormatTimestamp(90))
	require.Equal(t, "01:02:03", youtube.FormatTimestamp(3723))
	require.Equal(t, "02:00:00", youtube.FormatTimestamp(7200))
}

func TestBuildAudioWindows_LongVideoThreeWindows(t *testing.T) {
	got := youtube.BuildAudioWindows(60, 7200, 3)
	require.Equal(t, []youtube.AudioWindow{
		{Start: 0, End: 60},
		{Start: 3570, End: 3630},
		{Start: 7140, End: 7200},
	}, got)
}

func TestBuildAudioWindows_MaxWindowsOne(t *testing.T) {
	got := youtube.BuildAudioWindows(60, 7200, 1)
	require.Equal(t, []youtube.AudioWindow{{Start: 0, End: 60}}, got)
}

func TestBuildAudioWindows_MaxWindowsTwo(t *testing.T) {
	got := youtube.BuildAudioWindows(60, 7200, 2)
	require.Equal(t, []youtube.AudioWindow{
		{Start: 0, End: 60},
		{Start: 7140, End: 7200},
	}, got)
}

func TestBuildAudioWindows_ShortVideoReturnsWhole(t *testing.T) {
	got := youtube.BuildAudioWindows(60, 100, 3)
	require.Equal(t, []youtube.AudioWindow{{Start: 0, End: 100}}, got)
}

func TestBuildAudioWindows_UnknownDurationReturnsStart(t *testing.T) {
	got := youtube.BuildAudioWindows(60, 0, 3)
	require.Equal(t, []youtube.AudioWindow{{Start: 0, End: 60}}, got)
}

func TestBuildAudioWindows_DefaultsWindowAndMax(t *testing.T) {
	got := youtube.BuildAudioWindows(0, 7200, 0)
	require.Equal(t, []youtube.AudioWindow{
		{Start: 0, End: 60},
		{Start: 3570, End: 3630},
		{Start: 7140, End: 7200},
	}, got)
}
