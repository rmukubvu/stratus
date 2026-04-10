import { OperatorConnectionError } from "@/components/operator-connection-error";
import { PageHeader } from "@/components/page-header";
import { RequestTable } from "@/components/request-table";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { OperatorFetchError, getActivity } from "@/lib/operator";

export const dynamic = "force-dynamic";

type SearchParams = Promise<{
  service?: string;
  status?: string;
  q?: string;
}>;

export default async function ActivityPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const params = await searchParams;
  try {
    const activity = await getActivity({
      service: params.service,
      status: params.status,
      q: params.q,
      limit: "100",
    });

    return (
      <>
        <PageHeader
          eyebrow="Activity"
          title="Recent request stream"
          description="This is the browser companion to the Stratus terminal feed: normalized service classification, status, latency, and request IDs for the last in-memory window."
        />

        <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
          <CardHeader>
            <CardTitle>Filter recent activity</CardTitle>
          </CardHeader>
          <CardContent>
            <form className="grid gap-3 md:grid-cols-3">
              <Input name="service" placeholder="service e.g. lambda" defaultValue={params.service ?? ""} />
              <Input name="status" placeholder="status class e.g. 5xx" defaultValue={params.status ?? ""} />
              <div className="flex gap-3">
                <Input name="q" placeholder="text search" defaultValue={params.q ?? ""} />
                <Button type="submit">Apply</Button>
              </div>
            </form>
          </CardContent>
        </Card>

        <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
          <CardHeader>
            <CardTitle>Operator activity</CardTitle>
          </CardHeader>
          <CardContent>
            <RequestTable items={activity.items} />
          </CardContent>
        </Card>
      </>
    );
  } catch (error) {
    if (error instanceof OperatorFetchError) {
      return (
        <>
          <PageHeader
            eyebrow="Activity"
            title="Recent request stream"
            description="Point the portal at a real Stratus instance before using the operator pages."
          />
          <OperatorConnectionError endpoint={error.endpoint} detail={error.detail} />
        </>
      );
    }
    throw error;
  }
}
