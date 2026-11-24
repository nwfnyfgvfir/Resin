import { type ColumnDef, flexRender, getCoreRowModel, useReactTable } from "@tanstack/react-table";

type DataTableProps<T> = {
    data: T[];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    columns: ColumnDef<T, any>[];
    onRowClick?: (row: T) => void;
    selectedRowId?: string;
    getRowId?: (row: T) => string;
    className?: string;
    wrapClassName?: string;
    compactLabels?: Record<string, string>;
};

export function DataTable<T>({
    data,
    columns,
    onRowClick,
    selectedRowId,
    getRowId,
    className,
    wrapClassName,
    compactLabels,
}: DataTableProps<T>) {
    // TanStack Table returns mutable table helpers; React Compiler intentionally skips memoizing here.
    // eslint-disable-next-line react-hooks/incompatible-library
    const table = useReactTable({
        data,
        columns,
        getCoreRowModel: getCoreRowModel(),
        getRowId,
    });

    return (
        <div className={`data-table-wrap ${compactLabels ? "data-table-wrap-compact" : ""} ${wrapClassName ?? ""}`}>
            <table className={`data-table ${compactLabels ? "data-table-compact" : ""} ${className ?? ""}`} role="table">
                <thead role="rowgroup">
                    {table.getHeaderGroups().map((headerGroup) => (
                        <tr key={headerGroup.id} role="row">
                            {headerGroup.headers.map((header) => (
                                <th key={header.id} scope="col" data-column={compactLabels ? header.column.id : undefined}>
                                    {header.isPlaceholder ? null : flexRender(header.column.columnDef.header, header.getContext())}
                                </th>
                            ))}
                        </tr>
                    ))}
                </thead>
                <tbody role="rowgroup">
                    {table.getRowModel().rows.map((row) => {
                        const isSelected = selectedRowId != null && row.id === selectedRowId;
                        return (
                            <tr
                                key={row.id}
                                role="row"
                                className={isSelected ? "data-table-row-selected" : onRowClick ? "clickable-row" : undefined}
                                onClick={onRowClick ? () => onRowClick(row.original) : undefined}
                            >
                                {row.getVisibleCells().map((cell) => {
                                    const label = compactLabels?.[cell.column.id];
                                    return (
                                        <td key={cell.id} role="cell" data-column={compactLabels ? cell.column.id : undefined}>
                                            {label ? <span className="compact-cell-label">{label}</span> : null}
                                            {flexRender(cell.column.columnDef.cell, cell.getContext())}
                                        </td>
                                    );
                                })}
                            </tr>
                        );
                    })}
                </tbody>
            </table>
        </div>
    );
}
