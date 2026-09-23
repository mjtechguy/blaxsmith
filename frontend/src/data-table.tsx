import type { ReactNode } from "react";
import type { Header, ReactTable, RowData, TableFeatures } from "@tanstack/react-table";

type Props<TFeatures extends TableFeatures, TData extends RowData> = {
  table: ReactTable<TFeatures, TData>;
  label: string;
  empty?: string;
  header?: (header: Header<TFeatures, TData>) => ReactNode;
};

export function DataTable<TFeatures extends TableFeatures, TData extends RowData>({ table, label, empty, header: renderHeader }: Props<TFeatures, TData>) {
  return <div className="table-scroll" role="region" aria-label={`${label} table`} tabIndex={0}>
    <table aria-label={label}>
      <thead>{table.getHeaderGroups().map((group) => <tr key={group.id}>{group.headers.map((header) => {
        const column = header.column;
        const sorted = "getCanSort" in column && column.getCanSort() && "getIsSorted" in column ? column.getIsSorted() : null;
        return <th key={header.id} scope="col" aria-sort={sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : sorted === false ? "none" : undefined}>
          {renderHeader ? renderHeader(header) : <span className="table-label"><table.FlexRender header={header} /></span>}
        </th>;
      })}</tr>)}</thead>
      <tbody>{table.getRowModel().rows.map((row) => <tr key={row.id}>{row.getAllCells().map((cell) => <td key={cell.id}><table.FlexRender cell={cell} /></td>)}</tr>)}</tbody>
    </table>
    {empty && table.getRowModel().rows.length === 0 ? <div className="table-empty">{empty}</div> : null}
  </div>;
}
