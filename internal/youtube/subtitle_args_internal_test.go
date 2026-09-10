package youtube

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSubtitleArgs_IncludesIgnoreErrors(t *testing.T) {
	args := subtitleArgs("/tmp/out")

	require.Contains(t, args, "--ignore-errors",
		"без --ignore-errors падение русских авто-субтитров (429) обрывает скачивание английских")
	require.Contains(t, args, "--write-auto-sub")
	require.Contains(t, args, "--sub-lang")
	require.Contains(t, args, "-o")
	require.Contains(t, args, "/tmp/out")
}
