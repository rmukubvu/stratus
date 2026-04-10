import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

export function StatusBadge({ status }: { status: number }) {
  const tone =
    status >= 500
      ? "bg-rose-100 text-rose-700 border-rose-200"
      : status >= 400
        ? "bg-amber-100 text-amber-700 border-amber-200"
        : "bg-emerald-100 text-emerald-700 border-emerald-200";

  return (
    <Badge
      variant="outline"
      className={cn("rounded-full px-2.5 py-1 text-[11px] font-semibold", tone)}
    >
      {status}
    </Badge>
  );
}
