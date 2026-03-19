package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	_ "modernc.org/sqlite"
)

type viewMode int

const (
	modeTableView viewMode = iota
	modeCreateTable
	modeAddColumn
	modeEditCell
	modeDropColumn
	modeDropTable
)

type columnDef struct {
	name    string
	typ     string
	formula string
}

type model struct {
	db             *sql.DB
	tables         []string
	currentTableIdx int
	columns        []columnDef
	rows           [][]string
	totalRows      int
	viewportOffset int
	viewportHeight int
	cursorRow      int
	cursorCol      int
	mode           viewMode
	input          textinput.Model
	input2         textinput.Model
	editRow        int
	editCol        int
	addColName     string
	addColType     string
	addColStep     int
	clipboard      string
	err            string
	width          int
	height         int
}

func initDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	schema := `
	CREATE TABLE IF NOT EXISTS __tp_tables (name TEXT PRIMARY KEY);
	CREATE TABLE IF NOT EXISTS __tp_columns (
		table_name TEXT, col_name TEXT, col_type TEXT,
		formula TEXT, col_order INTEGER,
		PRIMARY KEY (table_name, col_name)
	);
	CREATE TABLE IF NOT EXISTS __tp_dependencies (
		table_name TEXT, col_name TEXT,
		depends_on_table TEXT, depends_on_col TEXT,
		PRIMARY KEY (table_name, col_name, depends_on_table, depends_on_col)
	);
	`
	_, err = db.Exec(schema)
	return db, err
}

func (m *model) loadTables() error {
	rows, err := m.db.Query("SELECT name FROM __tp_tables ORDER BY name")
	if err != nil {
		return err
	}
	defer rows.Close()

	m.tables = nil
	for rows.Next() {
		var name string
		rows.Scan(&name)
		m.tables = append(m.tables, name)
	}
	return nil
}

func (m *model) currentTable() string {
	if m.currentTableIdx >= 0 && m.currentTableIdx < len(m.tables) {
		return m.tables[m.currentTableIdx]
	}
	return ""
}

func (m *model) loadTableData() error {
	tableName := m.currentTable()
	if tableName == "" {
		m.columns = nil
		m.rows = nil
		m.totalRows = 0
		return nil
	}

	rows, err := m.db.Query("SELECT col_name, col_type, COALESCE(formula, ''), col_order FROM __tp_columns WHERE table_name = ? ORDER BY col_order", tableName)
	if err != nil {
		return err
	}
	defer rows.Close()

	m.columns = nil
	for rows.Next() {
		var cd columnDef
		var order int
		rows.Scan(&cd.name, &cd.typ, &cd.formula, &order)
		m.columns = append(m.columns, cd)
	}

	if len(m.columns) == 0 {
		m.rows = nil
		m.totalRows = 0
		return nil
	}

	// Get total row count
	var count int
	m.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", tableName)).Scan(&count)
	m.totalRows = count

	// Calculate viewport height (terminal height - header(1) - table header(1) - footer(1))
	m.viewportHeight = m.height - 3
	if m.viewportHeight < 5 {
		m.viewportHeight = 5
	}

	// Always scroll to bottom (show last rows)
	if m.totalRows > m.viewportHeight {
		m.viewportOffset = m.totalRows - m.viewportHeight
		m.cursorRow = m.viewportHeight - 1
	} else {
		m.viewportOffset = 0
		m.cursorRow = max(0, m.totalRows - 1)
	}

	// Load only visible rows
	return m.loadVisibleRows()
}

func (m *model) loadVisibleRows() error {
	tableName := m.currentTable()
	if tableName == "" || len(m.columns) == 0 {
		return nil
	}

	nonFormulaCols := []string{}
	for _, c := range m.columns {
		if c.typ != "FORMULA" {
			nonFormulaCols = append(nonFormulaCols, c.name)
		}
	}

	if len(nonFormulaCols) == 0 {
		m.rows = nil
		return nil
	}

	// Load rows with LIMIT and OFFSET
	query := fmt.Sprintf("SELECT %s FROM %s LIMIT ? OFFSET ?", strings.Join(nonFormulaCols, ", "), tableName)
	dataRows, err := m.db.Query(query, m.viewportHeight, m.viewportOffset)
	if err != nil {
		return err
	}
	defer dataRows.Close()

	m.rows = nil
	for dataRows.Next() {
		vals := make([]interface{}, len(nonFormulaCols))
		valPtrs := make([]interface{}, len(nonFormulaCols))
		for i := range vals {
			valPtrs[i] = &vals[i]
		}
		dataRows.Scan(valPtrs...)

		row := make([]string, len(m.columns))
		nonFormulaIdx := 0
		for i, col := range m.columns {
			if col.typ == "FORMULA" {
				row[i] = m.computeFormula(i, vals)
			} else {
				if col.typ == "DATE" {
					if vals[nonFormulaIdx] != nil {
						if epoch, ok := vals[nonFormulaIdx].(int64); ok {
							row[i] = time.Unix(epoch, 0).UTC().Format("2006-01-02 15:04:05")
						} else {
							row[i] = fmt.Sprintf("%v", vals[nonFormulaIdx])
						}
					}
				} else {
					if vals[nonFormulaIdx] != nil {
						row[i] = fmt.Sprintf("%v", vals[nonFormulaIdx])
					}
				}
				nonFormulaIdx++
			}
		}
		m.rows = append(m.rows, row)
	}

	// Reset cursor if out of bounds
	if m.cursorRow >= len(m.rows) {
		m.cursorRow = max(0, len(m.rows)-1)
	}
	if m.cursorCol >= len(m.columns) {
		m.cursorCol = max(0, len(m.columns)-1)
	}

	return nil
}

func (m *model) computeFormula(colIdx int, rowVals []interface{}) string {
	formula := m.columns[colIdx].formula
	if formula == "" {
		return ""
	}

	nonFormulaIdx := 0
	for _, col := range m.columns {
		if col.typ == "FORMULA" {
			continue
		}
		placeholder := fmt.Sprintf("this.%s", col.name)
		var val string
		if rowVals[nonFormulaIdx] != nil {
			val = fmt.Sprintf("%v", rowVals[nonFormulaIdx])
		} else {
			val = "NULL"
		}
		if col.typ == "TEXT" && val != "NULL" {
			val = fmt.Sprintf("'%s'", strings.ReplaceAll(val, "'", "''"))
		}
		formula = strings.ReplaceAll(formula, placeholder, val)
		nonFormulaIdx++
	}

	var result string
	err := m.db.QueryRow(formula).Scan(&result)
	if err != nil {
		return "ERR"
	}
	return result
}

func (m *model) createTable(name string) error {
	_, err := m.db.Exec("INSERT INTO __tp_tables (name) VALUES (?)", name)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(fmt.Sprintf("CREATE TABLE %s (id INTEGER PRIMARY KEY AUTOINCREMENT)", name))
	if err != nil {
		return err
	}
	_, err = m.db.Exec("INSERT INTO __tp_columns (table_name, col_name, col_type, col_order, formula) VALUES (?, 'id', 'INTEGER', 0, '')", name)
	return err
}

func (m *model) dropColumn(colIdx int) error {
	tableName := m.currentTable()
	if tableName == "" {
		return fmt.Errorf("no table selected")
	}
	
	if colIdx < 0 || colIdx >= len(m.columns) {
		return fmt.Errorf("invalid column index")
	}
	
	colName := m.columns[colIdx].name
	if colName == "id" {
		return fmt.Errorf("cannot drop id column")
	}
	
	// Delete from metadata
	_, err := m.db.Exec("DELETE FROM __tp_columns WHERE table_name = ? AND col_name = ?", tableName, colName)
	if err != nil {
		return err
	}
	
	// Delete dependencies
	m.db.Exec("DELETE FROM __tp_dependencies WHERE table_name = ? AND col_name = ?", tableName, colName)
	m.db.Exec("DELETE FROM __tp_dependencies WHERE depends_on_table = ? AND depends_on_col = ?", tableName, colName)
	
	// Drop from actual table if not FORMULA
	if m.columns[colIdx].typ != "FORMULA" {
		// SQLite doesn't support DROP COLUMN directly in older versions
		// We'll leave the column in the table but remove from metadata
		// Alternatively, rebuild the table without the column
	}
	
	return nil
}

func (m *model) dropTable(tableName string) error {
	if tableName == "" {
		return fmt.Errorf("no table specified")
	}
	
	// Delete from metadata
	_, err := m.db.Exec("DELETE FROM __tp_columns WHERE table_name = ?", tableName)
	if err != nil {
		return err
	}
	
	_, err = m.db.Exec("DELETE FROM __tp_dependencies WHERE table_name = ?", tableName)
	if err != nil {
		return err
	}
	
	_, err = m.db.Exec("DELETE FROM __tp_tables WHERE name = ?", tableName)
	if err != nil {
		return err
	}
	
	// Drop the actual table
	_, err = m.db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName))
	return err
}

func (m *model) copyCell() error {
	if m.cursorRow >= len(m.rows) || m.cursorCol >= len(m.columns) {
		return fmt.Errorf("no cell selected")
	}
	
	m.clipboard = m.rows[m.cursorRow][m.cursorCol]
	
	// Copy to system clipboard using xclip
	cmd := exec.Command("xclip", "-selection", "clipboard")
	cmd.Stdin = strings.NewReader(m.clipboard)
	cmd.Run() // Ignore errors if xclip not available
	
	return nil
}

func (m *model) pasteCell() error {
	if m.cursorRow >= len(m.rows) || m.cursorCol >= len(m.columns) {
		return fmt.Errorf("no cell selected")
	}
	
	// Try to get from system clipboard first
	cmd := exec.Command("xclip", "-selection", "clipboard", "-o")
	output, err := cmd.Output()
	if err == nil {
		m.clipboard = string(output)
	}
	
	// Paste the value
	if m.clipboard == "" {
		return nil
	}
	
	return m.updateCell(m.cursorRow, m.cursorCol, m.clipboard)
}

func (m *model) addColumn(name, typ, formula string) error {
	tableName := m.currentTable()
	if tableName == "" {
		return fmt.Errorf("no table selected")
	}

	order := len(m.columns)
	_, err := m.db.Exec("INSERT INTO __tp_columns (table_name, col_name, col_type, formula, col_order) VALUES (?, ?, ?, ?, ?)",
		tableName, name, typ, formula, order)
	if err != nil {
		return err
	}

	if typ != "FORMULA" {
		sqlType := typ
		if typ == "DATE" {
			sqlType = "INTEGER"
		}
		_, err = m.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, name, sqlType))
		if err != nil {
			return err
		}
	}

	if formula != "" {
		if err := m.checkCircularDependency(tableName, name, formula); err != nil {
			m.db.Exec("DELETE FROM __tp_columns WHERE table_name = ? AND col_name = ?", tableName, name)
			return err
		}
		m.extractDependencies(tableName, name, formula)
	}

	return nil
}

func (m *model) checkCircularDependency(tableName, colName, formula string) error {
	deps := m.parseDependencies(formula)
	visited := make(map[string]bool)
	return m.dfsCheckCycle(tableName, colName, deps, visited)
}

func (m *model) dfsCheckCycle(table, col string, newDeps map[string][]string, visited map[string]bool) error {
	key := table + "." + col
	if visited[key] {
		return fmt.Errorf("circular dependency detected")
	}
	visited[key] = true

	for depTable := range newDeps {
		rows, err := m.db.Query("SELECT table_name, col_name FROM __tp_dependencies WHERE depends_on_table = ?", depTable)
		if err != nil {
			continue
		}
		for rows.Next() {
			var t, c string
			rows.Scan(&t, &c)
			if err := m.dfsCheckCycle(t, c, newDeps, visited); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
	}
	return nil
}

func (m *model) parseDependencies(formula string) map[string][]string {
	deps := make(map[string][]string)
	re := regexp.MustCompile(`FROM\s+(\w+)`)
	matches := re.FindAllStringSubmatch(formula, -1)
	for _, match := range matches {
		if len(match) > 1 {
			deps[match[1]] = []string{}
		}
	}
	return deps
}

func (m *model) extractDependencies(tableName, colName, formula string) {
	deps := m.parseDependencies(formula)
	for depTable := range deps {
		m.db.Exec("INSERT OR IGNORE INTO __tp_dependencies (table_name, col_name, depends_on_table, depends_on_col) VALUES (?, ?, ?, ?)",
			tableName, colName, depTable, "")
	}
}

func (m *model) addRow() error {
	tableName := m.currentTable()
	if tableName == "" {
		return fmt.Errorf("no table selected")
	}

	cols := []string{}
	for _, c := range m.columns {
		if c.name != "id" && c.typ != "FORMULA" {
			cols = append(cols, c.name)
		}
	}
	if len(cols) == 0 {
		_, err := m.db.Exec(fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", tableName))
		if err != nil {
			return err
		}
	} else {
		placeholders := make([]string, len(cols))
		for i := range placeholders {
			placeholders[i] = "NULL"
		}
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", tableName, strings.Join(cols, ", "), strings.Join(placeholders, ", "))
		_, err := m.db.Exec(query)
		if err != nil {
			return err
		}
	}
	
	// Update total count
	m.totalRows++
	
	// Scroll to bottom to show new row
	if m.totalRows > m.viewportHeight {
		m.viewportOffset = m.totalRows - m.viewportHeight
		m.cursorRow = m.viewportHeight - 1
	} else {
		m.cursorRow = m.totalRows - 1
	}
	
	return nil
}

func (m *model) updateCell(row, col int, value string) error {
	tableName := m.currentTable()
	if tableName == "" {
		return fmt.Errorf("no table selected")
	}

	if m.columns[col].typ == "FORMULA" {
		return fmt.Errorf("cannot edit formula column")
	}

	idCol := -1
	for i, c := range m.columns {
		if c.name == "id" {
			idCol = i
			break
		}
	}
	if idCol == -1 {
		return fmt.Errorf("no id column")
	}

	// Get the actual row ID from the visible row
	if row >= len(m.rows) {
		return fmt.Errorf("invalid row")
	}

	var finalValue interface{}
	if value == "" {
		finalValue = nil
	} else if m.columns[col].typ == "DATE" {
		t, err := time.Parse("2006-01-02 15:04:05", value)
		if err != nil {
			t, err = time.Parse("2006-01-02", value)
			if err != nil {
				return fmt.Errorf("invalid date format")
			}
		}
		// Store UTC epoch directly without timezone conversion
		finalValue = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC).Unix()
	} else {
		finalValue = value
	}

	query := fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", tableName, m.columns[col].name)
	_, err := m.db.Exec(query, finalValue, m.rows[row][idCol])
	return err
}

func (m *model) deleteRow(row int) error {
	tableName := m.currentTable()
	if tableName == "" {
		return fmt.Errorf("no table selected")
	}

	idCol := -1
	for i, c := range m.columns {
		if c.name == "id" {
			idCol = i
			break
		}
	}
	if idCol == -1 {
		return fmt.Errorf("no id column")
	}

	// Get the actual row ID from the visible row
	if row >= len(m.rows) {
		return fmt.Errorf("invalid row")
	}

	query := fmt.Sprintf("DELETE FROM %s WHERE id = ?", tableName)
	_, err := m.db.Exec(query, m.rows[row][idCol])
	if err != nil {
		return err
	}

	// Update total count
	m.totalRows--
	
	// Adjust viewport if needed
	if m.viewportOffset > 0 && m.viewportOffset >= m.totalRows {
		m.viewportOffset = max(0, m.totalRows - m.viewportHeight)
	}

	return err
}

func initialModel(db *sql.DB) model {
	ti := textinput.New()
	ti.Focus()
	ti.Width = 100  // Will be updated based on terminal width

	ti2 := textinput.New()
	ti2.Width = 100  // Will be updated based on terminal width

	m := model{
		db:        db,
		mode:      modeTableView,
		input:     ti,
		input2:    ti2,
		cursorRow: 0,
		cursorCol: 0,
	}
	m.loadTables()
	if len(m.tables) > 0 {
		m.currentTableIdx = 0
		m.loadTableData()
	}
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, tea.EnterAltScreen, tea.EnableMouseCellMotion)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		
		// Update input widths to use full terminal width
		m.input.Width = m.width - 4  // Leave small margin
		m.input2.Width = m.width - 4
		
		oldViewportHeight := m.viewportHeight
		m.viewportHeight = m.height - 3
		if m.viewportHeight < 5 {
			m.viewportHeight = 5
		}
		// Adjust viewport offset if height changed
		if oldViewportHeight != m.viewportHeight && len(m.columns) > 0 {
			m.loadVisibleRows()
		}
		return m, nil

	case tea.MouseMsg:
		if m.mode == modeTableView && msg.Type == tea.MouseLeft {
			// Check if clicking on table tabs
			if msg.Y == 0 {
				x := 8
				for i, t := range m.tables {
					tabLen := len(t) + 4
					if msg.X >= x && msg.X < x+tabLen {
						m.currentTableIdx = i
						m.loadTableData()
						m.err = ""
						return m, nil
					}
					x += tabLen + 1
				}
				// Check [+ New Table] button
				if msg.X >= x && msg.X < x+13 {
					m.mode = modeCreateTable
					m.input.SetValue("")
					m.err = ""
					return m, nil
				}
			}
		}

	case tea.KeyMsg:
		if msg.String() == "ctrl+q" {
			return m, tea.Quit
		}

		if m.mode != modeTableView {
			return m.handleInputMode(msg)
		}

		switch msg.String() {
		case "tab":
			if len(m.tables) > 0 {
				m.currentTableIdx = (m.currentTableIdx + 1) % len(m.tables)
				m.viewportOffset = 0  // Will be adjusted in loadTableData
				m.loadTableData()
				m.cursorRow = 0
				m.cursorCol = 0
				m.err = ""
			}
		case "shift+tab":
			if len(m.tables) > 0 {
				m.currentTableIdx--
				if m.currentTableIdx < 0 {
					m.currentTableIdx = len(m.tables) - 1
				}
				m.viewportOffset = 0  // Will be adjusted in loadTableData
				m.loadTableData()
				m.cursorRow = 0
				m.cursorCol = 0
				m.err = ""
			}
		case "enter":
			if len(m.rows) > 0 && len(m.columns) > 0 {
				m.editRow = m.cursorRow
				m.editCol = m.cursorCol
				m.mode = modeEditCell
				if m.editRow < len(m.rows) && m.editCol < len(m.columns) {
					m.input.SetValue(m.rows[m.editRow][m.editCol])
					m.input.Width = m.width - 4  // Use full width
				}
				m.err = ""
			}
		case "left":
			if m.cursorCol > 0 {
				m.cursorCol--
			}
		case "right":
			if m.cursorCol < len(m.columns)-1 {
				m.cursorCol++
			}
		case "up":
			if m.cursorRow > 0 {
				m.cursorRow--
			} else if m.viewportOffset > 0 {
				// Scroll up
				m.viewportOffset--
				m.loadVisibleRows()
			}
		case "down":
			if m.cursorRow < len(m.rows)-1 {
				m.cursorRow++
			} else if m.viewportOffset + len(m.rows) < m.totalRows {
				// Scroll down
				m.viewportOffset++
				m.loadVisibleRows()
			}
		case "ctrl+c":
			if err := m.copyCell(); err != nil {
				m.err = err.Error()
			} else {
				m.err = ""
			}
		case "ctrl+v":
			if err := m.pasteCell(); err != nil {
				m.err = err.Error()
			} else {
				m.loadVisibleRows()
				m.err = ""
			}
		case "x":
			if len(m.columns) > 0 {
				m.mode = modeDropColumn
				m.err = ""
			}
		case "shift+t", "T":
			if m.currentTable() != "" {
				m.mode = modeDropTable
				m.err = ""
			}
		case "shift+n", "N":
			m.mode = modeCreateTable
			m.input.SetValue("")
			m.err = ""
		case "c":
			m.mode = modeAddColumn
			m.addColStep = 0
			m.addColName = ""
			m.addColType = ""
			m.input.SetValue("")
			m.input2.SetValue("")
			m.err = ""
		case "a":
			if err := m.addRow(); err != nil {
				m.err = err.Error()
			} else {
				m.loadVisibleRows()
				m.err = ""
			}
		case "d":
			if len(m.rows) > 0 {
				if err := m.deleteRow(m.cursorRow); err != nil {
					m.err = err.Error()
				} else {
					// Adjust cursor if we deleted the last visible row
					if m.cursorRow >= len(m.rows) && m.cursorRow > 0 {
						m.cursorRow--
					}
					m.loadVisibleRows()
					m.err = ""
				}
			}
		}
	}
	return m, nil
}

func (m model) handleInputMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeTableView
		m.err = ""
		return m, nil

	case "ctrl+enter":
		// Insert newline in input
		if m.mode == modeEditCell || (m.mode == modeAddColumn && m.addColStep == 2) {
			currentVal := ""
			if m.mode == modeEditCell || (m.mode == modeAddColumn && m.addColStep < 2) {
				currentVal = m.input.Value()
				m.input.SetValue(currentVal + "\n")
			} else {
				currentVal = m.input2.Value()
				m.input2.SetValue(currentVal + "\n")
			}
		}
		return m, nil

	case "enter":
		switch m.mode {
		case modeCreateTable:
			if err := m.createTable(m.input.Value()); err != nil {
				m.err = err.Error()
			} else {
				m.loadTables()
				m.currentTableIdx = len(m.tables) - 1
				m.loadTableData()
				m.mode = modeTableView
				m.err = ""
			}

		case modeAddColumn:
			if m.addColStep == 0 {
				m.addColName = m.input.Value()
				m.addColStep = 1
				m.input.SetValue("")
			} else if m.addColStep == 1 {
				m.addColType = strings.ToUpper(m.input.Value())
				if m.addColType == "FORMULA" {
					m.addColStep = 2
					m.input2.SetValue("")
					m.input2.Focus()
					m.input.Blur()
				} else {
					if err := m.addColumn(m.addColName, m.addColType, ""); err != nil {
						m.err = err.Error()
					} else {
						m.loadTableData()
						m.mode = modeTableView
						m.err = ""
					}
				}
			} else if m.addColStep == 2 {
				formula := m.input2.Value()
				if err := m.addColumn(m.addColName, "FORMULA", formula); err != nil {
					m.err = err.Error()
				} else {
					m.loadTableData()
					m.mode = modeTableView
					m.err = ""
					m.input.Focus()
					m.input2.Blur()
				}
			}

		case modeEditCell:
			if err := m.updateCell(m.editRow, m.editCol, m.input.Value()); err != nil {
				m.err = err.Error()
			} else {
				m.loadVisibleRows()
				m.mode = modeTableView
				m.err = ""
			}
			
		case modeDropColumn:
			if err := m.dropColumn(m.cursorCol); err != nil {
				m.err = err.Error()
			} else {
				m.loadTableData()
				m.mode = modeTableView
				m.err = ""
			}
			
		case modeDropTable:
			tableName := m.currentTable()
			if err := m.dropTable(tableName); err != nil {
				m.err = err.Error()
			} else {
				m.loadTables()
				if len(m.tables) > 0 {
					m.currentTableIdx = 0
					m.loadTableData()
				} else {
					m.currentTableIdx = 0
					m.columns = nil
					m.rows = nil
				}
				m.mode = modeTableView
				m.err = ""
			}
		}
		return m, nil

	default:
		var cmd tea.Cmd
		if m.mode == modeAddColumn && m.addColStep == 2 {
			m.input2, cmd = m.input2.Update(msg)
		} else {
			m.input, cmd = m.input.Update(msg)
		}
		return m, cmd
	}
}

func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	switch m.mode {
	case modeCreateTable:
		return m.renderInputScreen("Create Table", "Table name:", m.input.View())
	
	case modeAddColumn:
		if m.addColStep == 0 {
			return m.renderInputScreen("Add Column", "Column name:", m.input.View())
		} else if m.addColStep == 1 {
			return m.renderInputScreen("Add Column", "Column type (TEXT/INTEGER/REAL/DATE/FORMULA):", m.input.View())
		} else {
			return m.renderInputScreen("Add Formula Column", "Column: "+m.addColName+"\nFormula SQL:", m.input2.View())
		}
	
	case modeEditCell:
		title := "Edit Cell"
		if m.editCol < len(m.columns) {
			title = "Edit " + m.currentTable() + "." + m.columns[m.editCol].name
		}
		return m.renderInputScreen(title, "", m.input.View())
	
	case modeDropColumn:
		prompt := ""
		if m.cursorCol < len(m.columns) {
			prompt = fmt.Sprintf("Drop column '%s'?\nPress Enter to confirm, ESC to cancel", m.columns[m.cursorCol].name)
		}
		return m.renderInputScreen("Drop Column", prompt, "")
	
	case modeDropTable:
		prompt := ""
		if m.currentTable() != "" {
			prompt = fmt.Sprintf("Drop table '%s' and all its data?\nPress Enter to confirm, ESC to cancel", m.currentTable())
		}
		return m.renderInputScreen("Drop Table", prompt, "")
	}

	// modeTableView
	var content strings.Builder

	// Fixed header: Table tabs in one line (no background, just colored active table)
	content.WriteString("Tables: ")
	for i, t := range m.tables {
		if i == m.currentTableIdx {
			// Active table in subtle dark yellow
			activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
			content.WriteString(activeStyle.Render("[" + t + "]"))
		} else {
			content.WriteString("[" + t + "]")
		}
		content.WriteString(" ")
	}
	content.WriteString("[+ New Table]")
	
	// Pad to right edge with Ctrl+Q: quit
	currentLen := len("Tables: [+ New Table] Ctrl+Q: quit") + 2
	for _, t := range m.tables {
		currentLen += len(t) + 3
	}
	padding := m.width - currentLen
	if padding > 0 {
		content.WriteString(strings.Repeat(" ", padding))
	}
	content.WriteString("Ctrl+Q: quit")
	content.WriteString("\n")

	// Table content
	var tableContent strings.Builder
	if m.currentTable() != "" {
		if len(m.columns) > 0 {
			tableContent.WriteString(m.renderTable())
		} else {
			tableContent.WriteString("No columns. Press 'c' to add a column.\n")
		}
	} else {
		tableContent.WriteString("No tables. Click [+ New Table] to create one.\n")
	}

	if m.err != "" {
		tableContent.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("Error: " + m.err))
		tableContent.WriteString("\n")
	}

	// Count lines in table content (count actual newlines)
	tableContentStr := tableContent.String()
	tableLineCount := 1 // Start with 1 for the first line
	if tableContentStr != "" {
		tableLineCount = strings.Count(tableContentStr, "\n") + 1
	}
	
	content.WriteString(tableContentStr)

	// Add padding lines to push footer to bottom
	// Total height = header(1) + table content + padding + footer(1)
	usedLines := 1 + tableLineCount + 1 // header + content + footer
	paddingNeeded := m.height - usedLines + 1 // +1 to push footer down by one more line
	if paddingNeeded > 0 {
		for i := 0; i < paddingNeeded; i++ {
			content.WriteString("\n")
		}
	}

	// Fixed footer shortcuts with shaded background (full width) - no trailing newline
	shortcuts := "Tab: switch  ←→↑↓: navigate  Enter: edit  N: new table  c: add column  x: drop column  a: add row  d: delete row  T: drop table  Ctrl+C/V: copy/paste"
	
	footerStyle := lipgloss.NewStyle().Background(lipgloss.Color("237")).Width(m.width)
	content.WriteString(footerStyle.Render(shortcuts))

	return content.String()
}

func (m model) renderInputScreen(title, prompt, inputView string) string {
	var s strings.Builder
	
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229"))
	s.WriteString(titleStyle.Render(title))
	s.WriteString("\n\n")
	
	if prompt != "" {
		s.WriteString(prompt)
		s.WriteString("\n\n")
	}
	
	if inputView != "" {
		s.WriteString(inputView)
		s.WriteString("\n\n")
	}
	
	s.WriteString("[Enter] Save  [Ctrl+Enter] New line  [ESC] Cancel")
	
	if m.err != "" {
		s.WriteString("\n\n")
		s.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("Error: " + m.err))
	}
	
	return s.String()
}

func (m model) renderTable() string {
	var s strings.Builder
	
	// Column widths
	colWidths := make([]int, len(m.columns))
	for i, col := range m.columns {
		colWidths[i] = max(15, len(col.name)+2)
		for _, row := range m.rows {
			if i < len(row) {
				colWidths[i] = max(colWidths[i], len(row[i])+2)
			}
		}
	}
	
	// Calculate total width of table
	totalWidth := 0
	for i, width := range colWidths {
		totalWidth += width
		if i < len(colWidths)-1 {
			totalWidth += 1 // for separator
		}
	}
	
	// Header with background (no left separator)
	headerLine := ""
	for i, col := range m.columns {
		cell := truncate(col.name, colWidths[i]-2)
		headerLine += padRight(cell, colWidths[i])
		if i < len(m.columns)-1 {
			headerLine += " "  // Space instead of separator for header
		}
	}
	
	headerStyle := lipgloss.NewStyle().Background(lipgloss.Color("237")).Bold(true).Width(m.width)
	s.WriteString(headerStyle.Render(headerLine))
	s.WriteString("\n")
	
	// Rows (no separator line)
	normalStyle := lipgloss.NewStyle()
	selectedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Background(lipgloss.Color("240"))
	
	for rowIdx, row := range m.rows {
		for colIdx, cellValue := range row {
			cell := truncate(cellValue, colWidths[colIdx]-2)
			cellText := padRight(cell, colWidths[colIdx])
			
			if rowIdx == m.cursorRow && colIdx == m.cursorCol {
				s.WriteString(selectedStyle.Render(cellText))
			} else {
				s.WriteString(normalStyle.Render(cellText))
			}
			
			if colIdx < len(row)-1 {
				s.WriteString("│")
			}
		}
		s.WriteString("\n")
	}
	
	return s.String()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: teapoy <database.db>")
		os.Exit(1)
	}

	db, err := initDB(os.Args[1])
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	p := tea.NewProgram(initialModel(db), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}