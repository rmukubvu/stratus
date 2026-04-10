import { AlertTriangle, FolderTree, Timer, Waves } from "lucide-react";

import { OperatorConnectionError } from "@/components/operator-connection-error";
import { PageHeader } from "@/components/page-header";
import { RequestTable } from "@/components/request-table";
import { StatCard } from "@/components/stat-card";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { OperatorFetchError, getOverview } from "@/lib/operator";

export const dynamic = "force-dynamic";

export default async function OverviewPage() {
  try {
    const overview = await getOverview();

    return (
      <>
        <PageHeader
          eyebrow="Overview"
          title="Local operator view for your emulator"
          description="Inspect live emulator state without dropping to the CLI. This surface is intentionally operational, read-only, and focused on the last things Stratus actually did."
        />

        <section className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
          <StatCard label="Endpoint" value={overview.endpoint} hint="Current Stratus base URL" />
          <StatCard label="Requests" value={String(overview.total_requests)} hint="Recent in-memory operator window" />
          <StatCard label="Uptime" value={`${overview.uptime_seconds}s`} hint={`Log ${overview.log_level} / ${overview.log_format}`} />
          <StatCard label="Data Dir" value={overview.data_dir} hint="Local persistence root" />
        </section>

        <section className="grid gap-6 xl:grid-cols-[1.5fr_1fr]">
          <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Timer className="h-4 w-4 text-slate-500" />
                Request health
              </CardTitle>
            </CardHeader>
            <CardContent className="grid gap-3 md:grid-cols-3">
              <div className="rounded-2xl border border-emerald-200 bg-emerald-50 p-4">
                <p className="text-xs font-semibold uppercase tracking-[0.24em] text-emerald-700">
                  2xx
                </p>
                <p className="mt-2 text-3xl font-semibold text-emerald-900">
                  {overview.status_2xx}
                </p>
              </div>
              <div className="rounded-2xl border border-amber-200 bg-amber-50 p-4">
                <p className="text-xs font-semibold uppercase tracking-[0.24em] text-amber-700">
                  4xx
                </p>
                <p className="mt-2 text-3xl font-semibold text-amber-900">
                  {overview.status_4xx}
                </p>
              </div>
              <div className="rounded-2xl border border-rose-200 bg-rose-50 p-4">
                <p className="text-xs font-semibold uppercase tracking-[0.24em] text-rose-700">
                  5xx
                </p>
                <p className="mt-2 text-3xl font-semibold text-rose-900">
                  {overview.status_5xx}
                </p>
              </div>
            </CardContent>
          </Card>

          <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <FolderTree className="h-4 w-4 text-slate-500" />
                Top services
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {overview.top_services.length === 0 ? (
                <p className="text-sm text-slate-500">No request traffic yet.</p>
              ) : (
                overview.top_services.map((service) => (
                  <div key={service.service} className="flex items-center justify-between rounded-2xl border border-slate-200 bg-slate-50 px-4 py-3">
                    <span className="font-medium text-slate-800">{service.service}</span>
                    <Badge variant="outline" className="rounded-full border-slate-300 bg-white px-2.5 py-1 text-xs">
                      {service.count}
                    </Badge>
                  </div>
                ))
              )}
            </CardContent>
          </Card>
        </section>

        <section className="grid gap-6 xl:grid-cols-[1.5fr_1fr]">
          <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Waves className="h-4 w-4 text-slate-500" />
                Recent failures
              </CardTitle>
            </CardHeader>
            <CardContent>
              <RequestTable items={overview.recent_errors} />
            </CardContent>
          </Card>

          <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <AlertTriangle className="h-4 w-4 text-slate-500" />
                Operator stance
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-4 text-sm leading-6 text-slate-600">
              <p>
                This portal is a read-only local cockpit. It surfaces emulator activity,
                not a fake AWS console.
              </p>
              <p>
                Use the CLI or SDK to mutate state. Use this view to see whether the
                emulator is healthy, where requests are flowing, and what failed last.
              </p>
              <p>
                The logs page is intentionally CloudWatch-inspired so Lambda and event
                debugging feel familiar without copying the AWS Console outright.
              </p>
            </CardContent>
          </Card>
        </section>
      </>
    );
  } catch (error) {
    if (error instanceof OperatorFetchError) {
      return (
        <>
          <PageHeader
            eyebrow="Overview"
            title="Local operator view for your emulator"
            description="Point the portal at a real Stratus instance before using the operator pages."
          />
          <OperatorConnectionError endpoint={error.endpoint} detail={error.detail} />
        </>
      );
    }
    throw error;
  }
}
