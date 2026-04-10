import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusBadge } from "@/components/status-badge";
import { RequestRecord, formatDateTime, formatDuration } from "@/lib/operator";

export function RequestTable({ items }: { items: RequestRecord[] }) {
  return (
    <div className="overflow-hidden rounded-[1.5rem] border border-slate-200/70 bg-white/60">
      <Table>
        <TableHeader>
          <TableRow className="border-slate-200/70">
            <TableHead className="portal-kicker text-[10px]">Time</TableHead>
            <TableHead className="portal-kicker text-[10px]">Service</TableHead>
            <TableHead className="portal-kicker text-[10px]">Operation</TableHead>
            <TableHead className="portal-kicker text-[10px]">Status</TableHead>
            <TableHead className="portal-kicker text-[10px]">Duration</TableHead>
            <TableHead className="portal-kicker text-[10px]">Path</TableHead>
            <TableHead className="portal-kicker text-[10px]">Request ID</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.length === 0 ? (
            <TableRow className="border-slate-200/70">
              <TableCell colSpan={7} className="py-14 text-center text-slate-500">
                No operator events yet.
              </TableCell>
            </TableRow>
          ) : (
            items.map((item) => (
              <TableRow key={`${item.request_id}-${item.time}`} className="border-slate-200/60">
                <TableCell className="text-xs text-slate-500">
                  {formatDateTime(item.time)}
                </TableCell>
                <TableCell className="font-medium text-slate-900">{item.service || "unknown"}</TableCell>
                <TableCell className="text-slate-600">
                  {item.operation || "unclassified"}
                </TableCell>
                <TableCell>
                  <StatusBadge status={item.status} />
                </TableCell>
                <TableCell className="text-slate-600">
                  {formatDuration(item.duration_ms)}
                </TableCell>
                <TableCell className="max-w-[20rem] truncate font-mono text-xs text-slate-600">
                  {item.method} {item.path}
                </TableCell>
                <TableCell className="font-mono text-xs text-slate-500">
                  {item.request_id}
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
    </div>
  );
}
