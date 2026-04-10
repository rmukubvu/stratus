import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export function StatCard({
  label,
  value,
  hint,
}: {
  label: string;
  value: string;
  hint?: string;
}) {
  return (
    <Card className="portal-panel rounded-[1.8rem] border-white/70 bg-white/76">
      <CardHeader className="pb-2">
        <CardTitle className="portal-kicker text-slate-500">{label}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="text-3xl font-semibold tracking-[-0.04em] text-slate-950">
          {value}
        </div>
        {hint ? (
          <p className="max-w-xs text-sm leading-6 text-slate-500">{hint}</p>
        ) : null}
      </CardContent>
    </Card>
  );
}
