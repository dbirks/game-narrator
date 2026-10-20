import type { ReactNode } from "react";

export interface TableColumn<T> {
  key: string;
  header: string;
  /** The cell content for a row (ReactNode, so a row can carry a testid). */
  cell: (row: T) => ReactNode;
}

export interface TableProps<T> {
  caption: string;
  columns: readonly TableColumn<T>[];
  rows: readonly T[];
  /** The row key; rows must be referentially stable. */
  rowKey: (row: T) => string;
  testid?: string;
  /** Row testid factory (the suite addresses rows by `data-testid`). */
  rowTestid?: (row: T) => string;
}

/** The design's table (docs/design/plum-peach "Lines, radius"): 1px dashed
 *  --hair row separators, mono 11px uppercase headers. A real <table> with
 *  a caption, so the a11y contract and the e2e selectors both hold. */
export function Table<T>({
  caption,
  columns,
  rows,
  rowKey,
  testid = "kit-table",
  rowTestid,
}: TableProps<T>) {
  return (
    <table className="kit-table" data-testid={testid}>
      <caption>{caption}</caption>
      <thead>
        <tr>
          {columns.map((column) => (
            <th key={column.key} scope="col">
              {column.header}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {rows.map((row) => (
          <tr key={rowKey(row)} data-testid={rowTestid?.(row)}>
            {columns.map((column) => (
              <td key={column.key}>{column.cell(row)}</td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}
