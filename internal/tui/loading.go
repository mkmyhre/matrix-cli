package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type loadingTickMsg struct{}
type loadingDoneMsg struct{}

type loadingModel struct {
	message string
	frame   int
	done    <-chan struct{}
}

var loadingFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m loadingModel) Init() tea.Cmd {
	return tea.Batch(waitLoadingDone(m.done), loadingTick())
}

func waitLoadingDone(done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-done
		return loadingDoneMsg{}
	}
}

func loadingTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg { return loadingTickMsg{} })
}

func (m loadingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case loadingDoneMsg:
		return m, tea.Quit
	case loadingTickMsg:
		m.frame = (m.frame + 1) % len(loadingFrames)
		return m, loadingTick()
	}
	return m, nil
}

func (m loadingModel) View() string {
	return fmt.Sprintf("\n  %s %s\n", loadingFrames[m.frame], m.message)
}

// RunLoading keeps the terminal responsive while startup work is running.
func RunLoading(ctx context.Context, message string, done <-chan struct{}) error {
	_, err := tea.NewProgram(loadingModel{message: message, done: done}, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}
