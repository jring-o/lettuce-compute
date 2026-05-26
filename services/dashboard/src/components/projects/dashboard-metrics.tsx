import type { LeafStats } from "@/types/infrastructure";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { formatNumber } from "@/lib/utils";

function formatETC(
  total: number,
  completed: number,
  throughput: number,
): string {
  if (throughput <= 0) return "\u2014";
  const hoursRemaining = (total - completed) / throughput;
  if (hoursRemaining < 1 / 60) return "< 1 min";
  const hours = Math.floor(hoursRemaining);
  const minutes = Math.round((hoursRemaining - hours) * 60);
  if (hours === 0) return `${minutes}m`;
  return `${hours}h ${minutes}m`;
}

function agreementColor(rate: number): string {
  if (rate >= 0.95) return "text-green-600 dark:text-green-400";
  if (rate >= 0.8) return "text-yellow-600 dark:text-yellow-400";
  return "text-red-600 dark:text-red-400";
}

interface DashboardMetricsProps {
  stats: LeafStats;
  isOngoing: boolean;
}

export function DashboardMetrics({ stats, isOngoing }: DashboardMetricsProps) {
  const completionPct =
    stats.total_work_units > 0
      ? (stats.work_units_validated / stats.total_work_units) * 100
      : 0;

  const agreementRate = stats.agreement_rate ?? -1;

  return (
    <div
      data-testid="dashboard-metrics"
      className="grid grid-cols-2 gap-4 md:grid-cols-3 xl:grid-cols-6"
    >
      {/* Progress */}
      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-xs font-medium text-muted-foreground">
            Progress
          </CardTitle>
        </CardHeader>
        <CardContent>
          {isOngoing ? (
            <p className="text-2xl font-bold">
              {formatNumber(stats.work_units_validated)}{" "}
              <span className="text-sm font-normal text-muted-foreground">
                completed
              </span>
            </p>
          ) : (
            <>
              <p className="text-2xl font-bold">
                {formatNumber(stats.work_units_validated)}{" "}
                <span className="text-sm font-normal text-muted-foreground">
                  / {formatNumber(stats.total_work_units)}
                </span>
              </p>
              <Progress
                value={completionPct}
                className="mt-2"
                data-testid="progress-bar"
              />
            </>
          )}
        </CardContent>
      </Card>

      {/* Active Volunteers */}
      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-xs font-medium text-muted-foreground">
            Active Volunteers
          </CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-2xl font-bold">
            {formatNumber(stats.active_volunteers)}
          </p>
          <p className="text-xs text-muted-foreground">volunteering</p>
        </CardContent>
      </Card>

      {/* Completion Rate */}
      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-xs font-medium text-muted-foreground">
            Completion Rate
          </CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-2xl font-bold">
            {(stats.throughput_per_hour ?? 0).toFixed(1)}
            <span className="text-sm font-normal text-muted-foreground">
              /hr
            </span>
          </p>
        </CardContent>
      </Card>

      {/* ETC */}
      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-xs font-medium text-muted-foreground">
            Est. Completion
          </CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-2xl font-bold">
            {isOngoing
              ? "\u2014"
              : formatETC(
                  stats.total_work_units,
                  stats.work_units_validated,
                  stats.throughput_per_hour ?? 0,
                )}
          </p>
        </CardContent>
      </Card>

      {/* Agreement Rate */}
      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-xs font-medium text-muted-foreground">
            Agreement Rate
          </CardTitle>
        </CardHeader>
        <CardContent>
          {agreementRate < 0 ? (
            <p className="text-2xl font-bold">{"\u2014"}</p>
          ) : (
            <p
              className={`text-2xl font-bold ${agreementColor(agreementRate)}`}
              data-testid="agreement-rate"
            >
              {(agreementRate * 100).toFixed(1)}%
            </p>
          )}
        </CardContent>
      </Card>

      {/* Credit Granted */}
      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-xs font-medium text-muted-foreground">
            Credit Granted
          </CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-2xl font-bold">
            {formatNumber(stats.total_credit_granted)}
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
