import {
  ComposedChart,
  Line,
  Bar,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  Legend,
} from 'recharts';
import type { DailyStats } from '@/lib/api';

interface TrendChartProps {
  data: DailyStats[];
}

function formatDateLabel(value: string) {
  if (value.includes('T')) {
    const date = new Date(value);
    return new Intl.DateTimeFormat(undefined, {
      hour: 'numeric',
    }).format(date);
  }
  const [year, month, day] = value.split('-').map(Number);
  if (!year || !month || !day) return value;
  return new Intl.DateTimeFormat(undefined, {
    month: 'short',
    day: 'numeric',
  }).format(new Date(year, month - 1, day));
}

export function TrendChart({ data }: TrendChartProps) {
  if (!data || data.length === 0) {
    return (
      <div className="flex items-center justify-center h-[350px] text-muted-foreground">
        No data available
      </div>
    );
  }

  const chartData = data.map((d) => ({
    name: d.date,
    runs: d.run_count,
    rate: d.success_rate * 100,
  }));

  return (
    <ResponsiveContainer width="100%" height={350}>
      <ComposedChart data={chartData} margin={{ top: 20, right: 20, bottom: 20, left: 20 }}>
        <CartesianGrid strokeDasharray="3 3" vertical={false} stroke="var(--graphite)" opacity={0.3} />
        <XAxis 
          dataKey="name" 
          tickFormatter={formatDateLabel}
          stroke="var(--text-3)"
          fontSize={12}
          tickLine={false}
          axisLine={false}
        />
        <YAxis 
          yAxisId="left"
          stroke="var(--text-3)"
          fontSize={12}
          tickLine={false}
          axisLine={false}
          allowDecimals={false}
        />
        <YAxis 
          yAxisId="right"
          orientation="right"
          stroke="var(--text-3)"
          fontSize={12}
          tickLine={false}
          axisLine={false}
          domain={[0, 100]}
          tickFormatter={(v) => `${v}%`}
        />
        <Tooltip 
          contentStyle={{ 
            backgroundColor: 'var(--midnight)',
            borderColor: 'var(--graphite)',
            color: 'var(--text-1)',
            borderRadius: '8px',
          }}
          itemStyle={{ fontSize: '12px' }}
        />
        <Legend 
          verticalAlign="top" 
          align="right" 
          wrapperStyle={{ paddingBottom: '20px', fontSize: '12px' }}
        />
        <Bar 
          yAxisId="left" 
          dataKey="runs" 
          name="Run Volume" 
          fill="var(--cyan-glow)" 
          radius={[4, 4, 0, 0]} 
          opacity={0.6}
        />
        <Line 
          yAxisId="right" 
          type="monotone" 
          dataKey="rate" 
          name="Success Rate" 
          stroke="var(--success)" 
          strokeWidth={2}
          dot={false}
          activeDot={{ r: 4, strokeWidth: 0 }}
        />
      </ComposedChart>
    </ResponsiveContainer>
  );
}
