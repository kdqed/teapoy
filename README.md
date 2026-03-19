# HANDOFF.md - Teapoy TUI Airtable Clone

## What This Is
A single-file Go TUI (Terminal User Interface) Airtable clone using SQLite for storage. Named "teapoy" (small table in Indian English). Features viewport-based scrolling, formula columns, and a maximized vertical layout.

## Quick Start
```bash
go build teapoy.go
./teapoy myproject.db
```

## Architecture

### Single File: `teapoy.go` (~1100 lines)
- **TUI Framework**: bubbletea with lipgloss for styling
- **Database**: SQLite (modernc.org/sqlite - pure Go)
- **Clipboard**: xclip integration for copy/paste

### Dependencies
```go
github.com/charmbracelet/bubbletea
github.com/charmbracelet/bubbles/textinput
github.com/charmbracelet/lipgloss
modernc.org/sqlite
```

## Database Schema

### Metadata Tables (prefix: `__tp_`)

**`__tp_tables`** - Tracks all user tables
- `name TEXT PRIMARY KEY`

**`__tp_columns`** - Column definitions per table
- `table_name TEXT`
- `col_name TEXT`
- `col_type TEXT` → TEXT, INTEGER, REAL, DATE, FORMULA
- `formula TEXT` → SQL for FORMULA columns
- `col_order INTEGER`
- Primary key: `(table_name, col_name)`

**`__tp_dependencies`** - Formula dependency tracking
- `table_name TEXT`
- `col_name TEXT`
- `depends_on_table TEXT`
- `depends_on_col TEXT`
- Primary key: `(table_name, col_name, depends_on_table, depends_on_col)`

### User Tables
- Created dynamically via `CREATE TABLE`
- Always have `id INTEGER PRIMARY KEY AUTOINCREMENT`
- Columns added via `ALTER TABLE ADD COLUMN` (except FORMULA - those are virtual)

## Key Features

### 1. Date Columns (type: DATE)
- **Storage**: Unix epoch (INTEGER) in UTC
- **Display**: `2006-01-02 15:04:05` format
- **Input**: Accepts `2006-01-02 15:04:05` or `2006-01-02`
- No timezone conversion - stores and displays exactly as entered in UTC

### 2. Formula Columns (type: FORMULA)
- **Virtual** - no physical column in SQLite
- **Computed on read** by executing SQL with row values substituted
- **Syntax**: Use `this.columnName` to reference current row
- **Examples**:
  ```sql
  SELECT COUNT(*) FROM orders WHERE customer_id = this.id
  SELECT SUM(amount) FROM orders WHERE customer_id = this.id
  SELECT name FROM products WHERE id = this.product_id
  SELECT (strftime('%s', 'now') - this.signup_date) / 86400
  ```
- **Execution**: Replace `this.X` with actual values, then execute SQL
- **Circular dependency prevention**: DFS check before accepting formula

### 3. Viewport Scrolling (Critical for Performance)
- **Only loads visible rows** using SQL `LIMIT` and `OFFSET`
- **Viewport height**: `terminal_height - 3` (header + column header + footer)
- **Starts at bottom** - shows last rows on table open
- **Scroll on navigation**: Up/down at edges triggers load of previous/next row
- **Dynamic resize**: Recalculates viewport on terminal resize

### 4. Copy/Paste
- **Ctrl+C**: Copy current cell
- **Ctrl+V**: Paste to current cell
- Syncs with system clipboard via `xclip`

## UI Layout (Maximized Vertical Space)

```
Tables: [logs] [projects] [+ New Table]              Ctrl+Q: quit  ← Header (no bg)
id  date                project    hours  description                ← Column headers (gray bg, full width)
1   2026-03-02 00:00:00 EDNXT-S    3      Recon                      ← Rows (subtle highlight on selected cell)
2   2026-03-03 00:00:00 EDNXT      3      Analytics Events
...
Tab: switch  ←→↑↓: navigate  Enter: edit  N: new table  c: add...   ← Footer (gray bg, full width)
```

### Layout Principles
- **Header**: One line, active table in subtle color (3 - dark yellow/brown), no background
- **Column headers**: Gray background (237), full width, bold, space-separated (no `│`)
- **Rows**: Space between content and separators, subtle highlight (240 gray bg, 255 white fg)
- **Footer**: Gray background (237), full width, last line of terminal (no trailing newline)
- **No separators in headers** - only between column data in rows

## View Modes

### modeTableView (default)
Table browsing and navigation

### modeCreateTable
Full-screen prompt for table name
- Enter: create table
- ESC: cancel

### modeAddColumn
Multi-step full-screen flow:
1. Column name
2. Type (TEXT/INTEGER/REAL/DATE/FORMULA)
3. If FORMULA: SQL formula input (Ctrl+Enter for newline)

### modeEditCell
Full-screen cell editor
- Input width: terminal width - 4
- Ctrl+Enter: newline
- Enter: save

### modeDropColumn
Confirmation screen for dropping column

### modeDropTable
Confirmation screen for dropping table + all data + metadata

## Keyboard Shortcuts

**Navigation**
- Tab / Shift+Tab: Switch tables
- ←→↑↓: Navigate cells (scrolls viewport at edges)

**Editing**
- Enter: Edit current cell
- Ctrl+C: Copy cell
- Ctrl+V: Paste cell

**Table/Column Operations**
- N: New table
- c: Add column
- x: Drop column
- a: Add row
- d: Delete row
- T: Drop table

**General**
- Ctrl+Q: Quit
- ESC: Cancel current operation / return to table view

## Code Structure

### Model State
```go
type model struct {
    db             *sql.DB
    tables         []string
    currentTableIdx int
    columns        []columnDef
    rows           [][]string
    totalRows      int          // Total in DB
    viewportOffset int          // SQL OFFSET for pagination
    viewportHeight int          // How many rows fit on screen
    cursorRow      int          // Position in viewport (0-based)
    cursorCol      int
    mode           viewMode
    clipboard      string
    // ... input fields, error state
}
```

### Core Functions

**Data Loading**
- `loadTables()` - Query `__tp_tables`
- `loadTableData()` - Load column metadata, count total rows, calculate viewport, scroll to bottom
- `loadVisibleRows()` - Load only visible rows using `LIMIT viewportHeight OFFSET viewportOffset`
- `computeFormula(colIdx, rowVals)` - Substitute `this.X` and execute SQL

**CRUD**
- `createTable(name)` - Insert to `__tp_tables`, `CREATE TABLE`, add id column to `__tp_columns`
- `addColumn(name, typ, formula)` - Insert to `__tp_columns`, `ALTER TABLE` (if not FORMULA), check circular deps
- `dropColumn(colIdx)` - Delete from `__tp_columns` and `__tp_dependencies`
- `dropTable(tableName)` - Delete from all metadata tables, `DROP TABLE`
- `addRow()` - `INSERT` with NULLs, increment totalRows, scroll to show new row
- `updateCell(row, col, value)` - `UPDATE` by id, handle DATE conversion
- `deleteRow(row)` - `DELETE` by id, decrement totalRows, adjust viewport
- `copyCell()` / `pasteCell()` - Clipboard via xclip

**Dependency Management**
- `checkCircularDependency()` - DFS traversal to detect cycles
- `parseDependencies(formula)` - Extract `FROM tablename` references
- `extractDependencies()` - Store in `__tp_dependencies`

### BubbleTea Pattern
- `Init()` - Return enter alt screen + enable mouse
- `Update(msg)` - Route by mode, handle keys/mouse/resize
- `View()` - Render based on mode

## Critical Implementation Details

### Viewport Scrolling
When user presses down on last visible row:
```go
if m.cursorRow < len(m.rows)-1 {
    m.cursorRow++
} else if m.viewportOffset + len(m.rows) < m.totalRows {
    m.viewportOffset++
    m.loadVisibleRows()
}
```

### Footer Positioning
Calculate padding to push footer to exact bottom:
```go
usedLines := 1 + tableLineCount + 1 // header + content + footer
paddingNeeded := m.height - usedLines + 1 // +1 to push footer down
for i := 0; i < paddingNeeded; i++ {
    content.WriteString("\n")
}
```

### Date Handling (Critical - No Timezone Conversion)
```go
// Parsing input
t, _ := time.Parse("2006-01-02 15:04:05", value)
finalValue = time.Date(t.Year(), t.Month(), t.Day(), 
                       t.Hour(), t.Minute(), t.Second(), 
                       0, time.UTC).Unix()

// Displaying
row[i] = time.Unix(epoch, 0).UTC().Format("2006-01-02 15:04:05")
```

### Formula Execution
```go
// Replace this.columnName with actual values
for _, col := range m.columns {
    if col.typ == "FORMULA" { continue }
    placeholder := fmt.Sprintf("this.%s", col.name)
    var val string
    if rowVals[idx] != nil {
        val = fmt.Sprintf("%v", rowVals[idx])
    } else {
        val = "NULL"
    }
    if col.typ == "TEXT" && val != "NULL" {
        val = fmt.Sprintf("'%s'", strings.ReplaceAll(val, "'", "''"))
    }
    formula = strings.ReplaceAll(formula, placeholder, val)
}
// Execute the substituted SQL
m.db.QueryRow(formula).Scan(&result)
```

### Subtle Color Choices
- Active table: Color 3 (dark yellow/brown) - subtle, not bright
- Cell highlight: Background 240 (dark gray), Foreground 255 (white)
- Header/footer background: Color 237 (darker gray)

## Common Pitfalls to Avoid

1. **Don't load all rows** - Use viewport with LIMIT/OFFSET or performance tanks with 1000+ rows
2. **Reset viewport on table switch** - Set `viewportOffset = 0` in `loadTableData()` so it recalculates
3. **Update totalRows after add/delete** - Otherwise viewport logic breaks
4. **Never add timezone conversion to dates** - Store/display in UTC exactly as entered
5. **Footer must be last line** - No trailing `\n` after footer render
6. **Column headers need full-width background** - Use `.Width(m.width)` in lipgloss
7. **Track both cursorRow (viewport position) and viewportOffset (DB offset)** - Row editing uses cursorRow, but DB operations need the row's actual ID
8. **Handle FORMULA columns specially** - They don't exist in the table, only in `__tp_columns`

## Testing Scenarios

### Basic Operations
1. Create table `logs` with columns: date (DATE), project (TEXT), hours (INTEGER)
2. Add 20 rows with various dates
3. Navigate with arrows - should scroll viewport smoothly
4. Edit cells - dates should maintain format
5. Copy/paste between cells

### Formula Columns
1. Create `customers` table: id, name (TEXT)
2. Create `orders` table: id, customer_id (INTEGER), amount (INTEGER)
3. Add formula to `customers`: `order_count = SELECT COUNT(*) FROM orders WHERE customer_id = this.id`
4. Add rows to both tables
5. Verify formula updates as orders are added

### Circular Dependency
1. In table A, add formula referencing table B
2. In table B, try adding formula referencing table A
3. Should reject with error

### Viewport Edge Cases
1. Table with 5 rows on terminal with height 30 - footer should stay at bottom
2. Table with 100 rows - should only load ~25 visible rows
3. Scroll to top and bottom - should load previous/next rows smoothly
4. Resize terminal - viewport should recalculate

## Future Enhancements (Not Implemented)

- SQLite views support
- Multi-line text in cells
- Column reordering
- Sort by column
- Search/filter
- Export to CSV
- Undo/redo
- Better error messages
- Row selection (select multiple rows)

## Contact Context
User preferences:
- Can write code
- Wants brief, minimal explanations
- No unsolicited suggestions
- Prefers compact, maximized UI