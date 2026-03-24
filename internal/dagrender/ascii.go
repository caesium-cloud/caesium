// Package dagrender renders DAG visualizations in ASCII.
package dagrender

import (
	"fmt"
	"io"
	"strings"

	"github.com/caesium-cloud/caesium/internal/dag"
)

// Render writes an ASCII DAG visualization to the writer.
func Render(analysis *dag.Analysis, w io.Writer) error {
	if len(analysis.Steps) == 0 {
		_, err := fmt.Fprintln(w, "(empty DAG)")
		return err
	}

	layers := analysis.ExecutionOrder

	// Compute box widths per layer (widest step name + padding).
	layerWidths := make([]int, len(layers))
	for i, layer := range layers {
		for _, name := range layer {
			if len(name)+2 > layerWidths[i] {
				layerWidths[i] = len(name) + 2
			}
		}
	}

	// Find max rows across all layers.
	maxRows := 0
	for _, layer := range layers {
		if len(layer) > maxRows {
			maxRows = len(layer)
		}
	}

	// Render row by row.
	// Each step box takes 3 lines: top border, content, bottom border.
	// Between layers we draw arrows.
	const arrow = " --> "

	for row := 0; row < maxRows; row++ {
		// Top borders.
		if err := renderLine(w, layers, layerWidths, row, "top", arrow); err != nil {
			return err
		}
		// Content.
		if err := renderLine(w, layers, layerWidths, row, "mid", arrow); err != nil {
			return err
		}
		// Bottom borders.
		if err := renderLine(w, layers, layerWidths, row, "bot", arrow); err != nil {
			return err
		}
	}

	return nil
}

func renderLine(w io.Writer, layers [][]string, widths []int, row int, part string, sep string) error {
	var b strings.Builder

	for col, layer := range layers {
		width := widths[col]

		if row < len(layer) {
			name := layer[row]
			switch part {
			case "top":
				b.WriteString(topBorder(width))
			case "mid":
				b.WriteString(middleLine(name, width))
			case "bot":
				b.WriteString(bottomBorder(width))
			}
		} else {
			// Empty cell — pad to width + 2 (for box borders).
			b.WriteString(strings.Repeat(" ", width+2))
		}

		if col < len(layers)-1 {
			if row < len(layer) && part == "mid" {
				b.WriteString(sep)
			} else {
				b.WriteString(strings.Repeat(" ", len(sep)))
			}
		}
	}

	_, err := fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	return err
}

func topBorder(width int) string {
	return "\u250c" + strings.Repeat("\u2500", width) + "\u2510"
}

func middleLine(name string, width int) string {
	padding := width - len(name)
	left := padding / 2
	right := padding - left
	return "\u2502" + strings.Repeat(" ", left) + name + strings.Repeat(" ", right) + "\u2502"
}

func bottomBorder(width int) string {
	return "\u2514" + strings.Repeat("\u2500", width) + "\u2518"
}
