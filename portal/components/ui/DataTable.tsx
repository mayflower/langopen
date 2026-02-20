import type { ReactNode } from "react";

export type ColumnDef<T> = {
  key: string;
  header: string;
  width?: string;
  render: (row: T) => ReactNode;
};

export function DataTable<T extends { id?: string }>({
  columns,
  rows,
  rowKey,
  onRowClick,
  selectedRow,
  dense = false
}: {
  columns: ColumnDef<T>[];
  rows: T[];
  rowKey?: (row: T, idx: number) => string;
  onRowClick?: (row: T) => void;
  selectedRow?: (row: T) => boolean;
  dense?: boolean;
}) {
  return (
    <div className="ui-table-wrap">
      <table className={`ui-table ${dense ? "ui-table--dense" : ""}`}>
        <thead>
          <tr>
            {columns.map((col) => (
              <th key={col.key} style={col.width ? { width: col.width } : undefined}>
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, idx) => {
            const key = rowKey ? rowKey(row, idx) : row.id || `${idx}`;
            const isSelected = selectedRow ? selectedRow(row) : false;
            return (
              <tr
                key={key}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                className={[onRowClick ? "is-clickable" : "", isSelected ? "is-selected" : ""].filter(Boolean).join(" ")}
              >
                {columns.map((col) => (
                  <td key={col.key}>{col.render(row)}</td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
