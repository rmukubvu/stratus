import { AlertTriangle, Terminal } from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";

export function OperatorConnectionError({
  endpoint,
  detail,
}: {
  endpoint: string;
  detail?: string;
}) {
  return (
    <Card className="border border-amber-200 bg-amber-50/80 shadow-sm">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-amber-900">
          <AlertTriangle className="h-4 w-4" />
          Portal is not connected to Stratus
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 text-sm leading-6 text-amber-950/80">
        <p>
          The portal tried to reach the operator API at{" "}
          <code className="rounded bg-white/70 px-1.5 py-0.5 font-mono text-xs">
            {endpoint}
          </code>
          , but that endpoint does not look like a Stratus build with the
          operator API enabled.
        </p>
        <div className="rounded-2xl border border-amber-200 bg-white/80 p-4">
          <p className="mb-2 flex items-center gap-2 font-medium text-amber-950">
            <Terminal className="h-4 w-4" />
            Run the portal against a live Stratus instance
          </p>
          <pre className="overflow-x-auto whitespace-pre-wrap font-mono text-xs text-slate-800">
{`cd /Users/robson/awsdev/stratus-git
go build -o /tmp/stratus-bin ./cmd/stratus
/tmp/stratus-bin --port 4566 --data-dir /tmp/stratus-data --log-format pretty --log-level debug

cd /Users/robson/awsdev/stratus-git/portal
STRATUS_BASE_URL=http://127.0.0.1:4566 npm run dev`}
          </pre>
        </div>
        {detail ? (
          <>
            <Separator />
            <div>
              <p className="mb-2 font-medium text-amber-950">Raw backend response</p>
              <pre className="overflow-x-auto whitespace-pre-wrap rounded-2xl border border-amber-200 bg-white/80 p-4 font-mono text-xs text-slate-800">
                {detail}
              </pre>
            </div>
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}
