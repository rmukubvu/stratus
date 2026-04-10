import { OperatorConnectionError } from "@/components/operator-connection-error";
import { PageHeader } from "@/components/page-header";
import { RequestTable } from "@/components/request-table";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { OperatorFetchError, getErrors } from "@/lib/operator";

export const dynamic = "force-dynamic";

export default async function ErrorsPage() {
  try {
    const errors = await getErrors();

    return (
      <>
        <PageHeader
          eyebrow="Errors"
          title="Recent failures and unsupported paths"
          description="Use this page to find the last request that failed, the AWS-shaped error Stratus returned, and the request ID you need to trace the issue through logs."
        />

        <Card className="border border-slate-200/80 bg-white/90 shadow-sm">
          <CardHeader>
            <CardTitle>Recent failing requests</CardTitle>
          </CardHeader>
          <CardContent>
            <RequestTable items={errors.items} />
          </CardContent>
        </Card>
      </>
    );
  } catch (error) {
    if (error instanceof OperatorFetchError) {
      return (
        <>
          <PageHeader
            eyebrow="Errors"
            title="Recent failures and unsupported paths"
            description="Point the portal at a real Stratus instance before using the operator pages."
          />
          <OperatorConnectionError endpoint={error.endpoint} detail={error.detail} />
        </>
      );
    }
    throw error;
  }
}
