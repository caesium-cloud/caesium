// Package dagrender renders DAG visualizations in ASCII.
package dagrender

import (
	"fmt"
	"io"
	"strings"

	"github.com/caesium-cloud/caesium/internal/dag"
)

const connGap = 6 // horizontal gap between layer columns for connectors

// Render writes an ASCII DAG visualization to the writer.
func Render(analysis *dag.Analysis, w io.Writer) error {
	if len(analysis.Steps) == 0 || len(analysis.ExecutionOrder) == 0 {
		_, err := fmt.Fprintln(w, "(empty DAG)")
		return err
	}

	layers := analysis.ExecutionOrder

	// Per-layer box content width (widest step name + 2 padding).
	layerWidths := make([]int, len(layers))
	for i, layer := range layers {
		for _, name := range layer {
			if w := len(name) + 2; w > layerWidths[i] {
				layerWidths[i] = w
			}
		}
	}

	maxRows := 0
	for _, layer := range layers {
		if len(layer) > maxRows {
			maxRows = len(layer)
		}
	}

	// Row offsets: centre each layer's steps vertically.
	rowOffset := make([]int, len(layers))
	for i, layer := range layers {
		rowOffset[i] = (maxRows - len(layer)) / 2
	}

	// Map step name → (layer, absolute row).
	stepPos := make(map[string][2]int)
	for i, layer := range layers {
		for j, name := range layer {
			stepPos[name] = [2]int{i, rowOffset[i] + j}
		}
	}

	// X positions for each layer column.
	boxW := func(col int) int { return layerWidths[col] + 2 } // +2 for │ borders
	layerX := make([]int, len(layers))
	x := 0
	for i := range layers {
		layerX[i] = x
		x += boxW(i)
		if i < len(layers)-1 {
			x += connGap
		}
	}

	// Canvas dimensions.
	canvasH := maxRows*3 + max(maxRows-1, 0) // 3 lines per box + 1 gap between rows
	if canvasH < 3 {
		canvasH = 3
	}
	c := newCanvas(x, canvasH)

	// Draw boxes.
	for i, layer := range layers {
		for j, name := range layer {
			row := rowOffset[i] + j
			c.writeBox(layerX[i], rowToY(row), name, layerWidths[i])
		}
	}

	// Build successor lookup.
	succs := make(map[string][]string)
	for _, s := range analysis.Steps {
		succs[s.Name] = s.Successors
	}

	// Draw connectors between adjacent layers.
	for i := 0; i < len(layers)-1; i++ {
		startX := layerX[i] + boxW(i) // first char after source box
		endX := layerX[i+1]           // first char of target box
		midX := (startX + endX) / 2   // vertical junction column
		arrowX := endX - 1            // arrowhead '>' position

		// Collect unique (sourceRow, targetRow) edges.
		type edge struct{ from, to int }
		seen := map[edge]bool{}
		var edges []edge
		for _, srcName := range layers[i] {
			srcRow := stepPos[srcName][1]
			for _, succName := range succs[srcName] {
				if pos, ok := stepPos[succName]; ok && pos[0] == i+1 {
					e := edge{srcRow, pos[1]}
					if !seen[e] {
						seen[e] = true
						edges = append(edges, e)
					}
				}
			}
		}

		// Fallback: if no explicit edges, assume all-to-all (sequential layers).
		if len(edges) == 0 {
			for _, srcName := range layers[i] {
				for _, tgtName := range layers[i+1] {
					edges = append(edges, edge{stepPos[srcName][1], stepPos[tgtName][1]})
				}
			}
		}

		// Group edges by source row and target row.
		sourceRows := map[int]bool{}
		targetRows := map[int]bool{}
		for _, e := range edges {
			sourceRows[e.from] = true
			targetRows[e.to] = true
		}

		// Draw horizontal lines from each source to the junction column.
		for row := range sourceRows {
			y := rowMidY(row)
			c.hLine(startX, midX-1, y, '─')
		}

		// Draw horizontal lines from the junction column to each target.
		for row := range targetRows {
			y := rowMidY(row)
			c.hLine(midX+1, arrowX-1, y, '─')
			c.set(arrowX, y, '>')
		}

		// Draw the vertical junction line and branch characters.
		allRows := map[int]bool{}
		for row := range sourceRows {
			allRows[row] = true
		}
		for row := range targetRows {
			allRows[row] = true
		}

		// Find the min and max rows that need vertical connection.
		minRow, maxRow := maxRows, -1
		for row := range allRows {
			if row < minRow {
				minRow = row
			}
			if row > maxRow {
				maxRow = row
			}
		}

		if minRow == maxRow {
			// Single row — straight horizontal arrow, just fill the junction.
			c.set(midX, rowMidY(minRow), '─')
		} else {
			// Draw vertical spine between min and max rows.
			minY := rowMidY(minRow)
			maxY := rowMidY(maxRow)
			c.vLine(midX, minY, maxY, '│')

			// Set junction characters at each connected row.
			for row := range allRows {
				y := rowMidY(row)
				hasLeft := sourceRows[row]
				hasRight := targetRows[row]
				isTop := row == minRow
				isBot := row == maxRow

				c.set(midX, y, junctionChar(hasLeft, hasRight, !isTop, !isBot))
			}
		}
	}

	return c.writeTo(w)
}

// junctionChar returns the box-drawing character for a junction point.
func junctionChar(left, right, up, down bool) rune {
	switch {
	case left && right && up && down:
		return '┼'
	case left && right && down:
		return '┬'
	case left && right && up:
		return '┴'
	case left && up && down:
		return '┤'
	case right && up && down:
		return '├'
	case left && right:
		return '─'
	case up && down:
		return '│'
	case left && down:
		return '┐'
	case left && up:
		return '┘'
	case right && down:
		return '┌'
	case right && up:
		return '└'
	default:
		return '─'
	}
}

// rowToY converts a grid row index to the top y-coordinate of the box.
func rowToY(row int) int {
	return row * 4 // 3 lines per box + 1 gap
}

// rowMidY returns the y-coordinate of the middle line of a box at the given row.
func rowMidY(row int) int {
	return rowToY(row) + 1
}

// ---------------------------------------------------------------------------
// Canvas — a 2D grid of runes for building the ASCII output.
// ---------------------------------------------------------------------------

type canvas struct {
	grid [][]rune
	w, h int
}

func newCanvas(w, h int) *canvas {
	grid := make([][]rune, h)
	for i := range grid {
		row := make([]rune, w)
		for j := range row {
			row[j] = ' '
		}
		grid[i] = row
	}
	return &canvas{grid: grid, w: w, h: h}
}

func (c *canvas) set(x, y int, r rune) {
	if y >= 0 && y < c.h && x >= 0 && x < c.w {
		c.grid[y][x] = r
	}
}

func (c *canvas) hLine(x1, x2, y int, r rune) {
	for x := x1; x <= x2; x++ {
		c.set(x, y, r)
	}
}

func (c *canvas) vLine(x, y1, y2 int, r rune) {
	if y1 > y2 {
		y1, y2 = y2, y1
	}
	for y := y1; y <= y2; y++ {
		c.set(x, y, r)
	}
}

func (c *canvas) writeBox(x, y int, name string, width int) {
	c.set(x, y, '┌')
	c.hLine(x+1, x+width, y, '─')
	c.set(x+width+1, y, '┐')

	c.set(x, y+1, '│')
	padding := width - len(name)
	left := padding / 2
	for i, ch := range name {
		c.set(x+1+left+i, y+1, ch)
	}
	c.set(x+width+1, y+1, '│')

	c.set(x, y+2, '└')
	c.hLine(x+1, x+width, y+2, '─')
	c.set(x+width+1, y+2, '┘')
}

func (c *canvas) writeTo(w io.Writer) error {
	for _, row := range c.grid {
		line := strings.TrimRight(string(row), " ")
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
