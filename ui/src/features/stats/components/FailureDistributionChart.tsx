import { PieChart, Pie, Cell, Tooltip, Legend, ResponsiveContainer } from 'recharts';
import type { FailingJob } from '@/lib/api';

const COLORS = [
  'hsl(221, 83%, 53%)',  // blue
  'hsl(262, 83%, 58%)',  // violet
  'hsl(330, 81%, 60%)',  // pink
  'hsl(24, 95%, 53%)',   // orange
  'hsl(142, 71%, 45%)',  // green
  'hsl(47, 96%, 53%)',   // yellow
  'hsl(189, 94%, 43%)',  // cyan
  'hsl(0, 72%, 51%)',    // red
];

interface FailureDistributionChartProps {
  data: FailingJob[];
}

export function FailureDistributionChart({ data }: FailureDistributionChartProps) {
  if (!data || data.length === 0) {
    return (
      <div className="flex items-center justify-center h-[300px] text-muted-foreground">
        No failures recorded
      </div>
    );
  }

  const chartData = data.map((job) => ({
    name: job.alias || job.job_id,
    value: job.failure_count,
  }));

  return (
    <ResponsiveContainer width="100%" height={300}>
      <PieChart>
        <Pie
          data={chartData}
          cx="50%"
          cy="50%"
          labelLine={false}
          outerRadius={100}
          dataKey="value"
          nameKey="name"
          label={({ name, percent }: { name?: string; percent?: number }) => `${name ?? ''} (${((percent ?? 0) * 100).toFixed(0)}%)`}
        >
          {chartData.map((_entry, index) => (
            <Cell key={`cell-${index}`} fill={COLORS[index % COLORS.length]} />
          ))}
        </Pie>
        <Tooltip />
        <Legend />
      </PieChart>
    </ResponsiveContainer>
  );
}
