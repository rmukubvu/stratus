import Link from "next/link";

import { LogViewer } from "@/components/log-viewer";
import { OperatorConnectionError } from "@/components/operator-connection-error";
import { PageHeader } from "@/components/page-header";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  formatDateTime,
  getLogEvents,
  getLogGroups,
  getLogStreams,
  OperatorFetchError,
} from "@/lib/operator";

export const dynamic = "force-dynamic";

type SearchParams = Promise<{
  group?: string;
  stream?: string;
}>;

export default async function LogsPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const params = await searchParams;
  try {
    const groups = await getLogGroups();
    const group =
      params.group ?? groups.items.at(0)?.name ?? "";
    const streams = await getLogStreams(group);
    const stream =
      params.stream ?? streams.items.at(0)?.stream_name ?? "";
    const events = await getLogEvents(group, stream);

    return (
      <>
        <PageHeader
          eyebrow="Logs"
          title="CloudWatch-style local log browsing"
          description="Browse log groups and streams directly from Stratus. This view is intentionally familiar for Lambda and event-driven debugging without recreating the AWS Console."
        />

        <section className="grid gap-6 xl:grid-cols-[320px_320px_1fr]">
          <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
            <CardHeader>
              <CardTitle>Log groups</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {groups.items.length === 0 ? (
                <p className="text-sm text-slate-500">No log groups yet.</p>
              ) : (
                groups.items.map((item) => (
                  <Link
                    key={item.name}
                    href={`/logs?group=${encodeURIComponent(item.name)}`}
                    className={`block rounded-2xl border px-4 py-3 transition ${
                      item.name === group
                        ? "border-slate-950 bg-slate-950 text-white"
                        : "border-slate-200 bg-slate-50 text-slate-700 hover:border-slate-300"
                    }`}
                  >
                    <p className="truncate font-medium">{item.name}</p>
                    <p className="mt-1 text-xs opacity-75">
                      Created {formatDateTime(item.created_at)}
                    </p>
                  </Link>
                ))
              )}
            </CardContent>
          </Card>

          <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
            <CardHeader>
              <CardTitle>Streams</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {streams.items.length === 0 ? (
                <p className="text-sm text-slate-500">No streams for this group.</p>
              ) : (
                streams.items.map((item) => (
                  <Link
                    key={item.stream_name}
                    href={`/logs?group=${encodeURIComponent(group)}&stream=${encodeURIComponent(item.stream_name)}`}
                    className={`block rounded-2xl border px-4 py-3 transition ${
                      item.stream_name === stream
                        ? "border-slate-950 bg-slate-950 text-white"
                        : "border-slate-200 bg-slate-50 text-slate-700 hover:border-slate-300"
                    }`}
                  >
                    <div className="flex items-center justify-between gap-2">
                      <p className="truncate font-medium">{item.stream_name}</p>
                      <Badge variant="outline" className="rounded-full border-current/20 bg-white/10 px-2 py-0.5 text-[11px]">
                        {item.stored_bytes}B
                      </Badge>
                    </div>
                    <p className="mt-1 text-xs opacity-75">
                      Last event {item.last_event_time ? formatDateTime(item.last_event_time) : "none"}
                    </p>
                  </Link>
                ))
              )}
            </CardContent>
          </Card>

          <LogViewer events={events.items} group={group} stream={stream} />
        </section>
      </>
    );
  } catch (error) {
    if (error instanceof OperatorFetchError) {
      return (
        <>
          <PageHeader
            eyebrow="Logs"
            title="CloudWatch-style local log browsing"
            description="Point the portal at a real Stratus instance before using the operator pages."
          />
          <OperatorConnectionError endpoint={error.endpoint} detail={error.detail} />
        </>
      );
    }
    throw error;
  }
}
