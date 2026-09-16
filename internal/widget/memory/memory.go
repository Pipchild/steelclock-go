package memory

import (
	"fmt"
	"image"
	"sync"

	"github.com/pozitronik/steelclock-go/internal/config"
	"github.com/pozitronik/steelclock-go/internal/metrics"
	"github.com/pozitronik/steelclock-go/internal/shared"
	"github.com/pozitronik/steelclock-go/internal/shared/render"
	"github.com/pozitronik/steelclock-go/internal/shared/util"
	"github.com/pozitronik/steelclock-go/internal/widget"
)

func init() {
	widget.Register("memory", func(cfg config.WidgetConfig) (widget.Widget, error) {
		return New(cfg)
	})
}

// Widget displays RAM usage
type Widget struct {
	*widget.BaseWidget
	mu             sync.RWMutex
	strategy       render.MetricDisplayStrategy
	Renderer       *render.MetricRenderer
	displayMode    render.DisplayMode
	currentValue   float64
	usedGB         float64
	totalGB        float64
	history        *util.RingBuffer[float64]
	textFormat     string
	memoryProvider metrics.MemoryProvider
}

// New creates a new memory widget
func New(cfg config.WidgetConfig) (*Widget, error) {
	base := widget.NewBaseWidget(cfg)
	helper := shared.NewConfigHelper(cfg)

	// Build common metric renderer (shared with CPU widget)
	mr, err := helper.BuildMetricRenderer()
	if err != nil {
		return nil, err
	}

	textFormat := "%.0f"
	if cfg.Text != nil && cfg.Text.Format != "" {
		textFormat = cfg.Text.Format
	}

	return &Widget{
		BaseWidget:     base,
		strategy:       mr.Strategy,
		Renderer:       mr.Renderer,
		displayMode:    mr.DisplayMode,
		history:        util.NewRingBuffer[float64](mr.HistoryLen),
		textFormat:     textFormat,
		memoryProvider: metrics.DefaultMemory,
	}, nil
}

// Update updates the memory usage
func (w *Widget) Update() error {
	percent, err := w.memoryProvider.UsedPercent()
	if err != nil {
		return err
	}

	// Clamp to 0-100
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}

	// Best-effort: GB figures are a display nicety, not worth failing Update() over.
	usedGB, totalGB, gbErr := w.memoryProvider.UsedGB()

	w.mu.Lock()
	defer w.mu.Unlock()

	w.currentValue = percent
	if gbErr == nil {
		w.usedGB = usedGB
		w.totalGB = totalGB
	}
	if w.displayMode == render.DisplayModeGraph {
		w.history.Push(percent)
	}

	return nil
}

// GetValue returns the current memory usage percentage (thread-safe)
func (w *Widget) GetValue() float64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.currentValue
}

// Render creates an image of the memory widget
func (w *Widget) Render() (image.Image, error) {
	// Create canvas with background and border
	img := w.CreateCanvas()
	w.ApplyBorder(img)

	// Get content area (adjusted for padding) and full bounds for gauge
	content := w.GetContentArea()
	pos := w.GetPosition()

	w.mu.RLock()
	defer w.mu.RUnlock()

	// In text mode, a format string with two value verbs (e.g. "%.1fGB (%.0f%%)")
	// renders used-GB and percent together instead of just the percentage.
	if w.displayMode == render.DisplayModeText && countFormatVerbs(w.textFormat) >= 2 {
		text := fmt.Sprintf(w.textFormat, w.usedGB, w.currentValue)
		w.Renderer.RenderText(img, text)
		return img, nil
	}

	// Delegate rendering to strategy
	w.strategy.Render(img, render.MetricData{
		Value:       w.currentValue,
		History:     w.history.ToSlice(),
		TextFormat:  w.textFormat,
		ContentArea: image.Rect(content.X, content.Y, content.X+content.Width, content.Y+content.Height),
		GaugeArea:   image.Rect(0, 0, pos.W, pos.H),
	}, w.Renderer)

	return img, nil
}

// countFormatVerbs counts fmt verb specifiers in a format string, treating a
// literal "%%" as zero verbs. Used to detect whether a configured text format
// wants one value (percent, the default) or two (used-GB and percent).
func countFormatVerbs(format string) int {
	count := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			i++ // literal "%%": skip both, not a verb
			continue
		}
		count++
	}
	return count
}
