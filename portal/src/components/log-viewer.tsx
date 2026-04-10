import { ScrollArea } from "@/components/ui/scroll-area";
import { EventSummary, formatDateTime } from "@/lib/operator";

export function LogViewer({
  events,
  group,
  stream,
}: {
  events: EventSummary[];
  group?: string;
  stream?: string;
}) {
  return (
    <div className="overflow-hidden rounded-[2rem] border border-slate-900/8 bg-[linear-gradient(180deg,#0f172a_0%,#111827_100%)] text-slate-100 shadow-[0_30px_80px_rgba(15,23,42,0.22)]">
      <div className="border-b border-white/8 px-6 py-5">
        <p className="text-[11px] font-semibold uppercase tracking-[0.3em] text-slate-400">
          CloudWatch-style log stream
        </p>
        <h3 className="mt-3 text-base font-medium tracking-[-0.02em] text-slate-100">
          {group && stream ? `${group} / ${stream}` : "Select a group and stream"}
        </h3>
      </div>
      <ScrollArea className="h-[34rem]">
        <div className="space-y-3 px-6 py-5 font-mono text-xs">
          {events.length === 0 ? (
            <p className="text-slate-500">No log events to display.</p>
          ) : (
            events.map((event, index) => (
              <div
                key={`${event.timestamp}-${index}`}
                className="grid grid-cols-[140px_1fr] gap-4 rounded-2xl border border-white/6 bg-white/[0.03] px-4 py-3"
              >
                <span className="text-slate-500">
                  {formatDateTime(event.timestamp)}
                </span>
                <pre className="whitespace-pre-wrap break-words text-slate-100">
                  {event.message}
                </pre>
              </div>
            ))
          )}
        </div>
      </ScrollArea>
    </div>
  );
}
