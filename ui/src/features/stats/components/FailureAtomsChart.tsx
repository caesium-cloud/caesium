import {
  BarChart,
  Bar,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  Cell,
} from 'recharts';
import type { FailingAtom } from '@/lib/api';

interface FailureAtomsChartProps {
  data: FailingAtom[];
}

export function FailureAtomsChart({ data }: FailureAtomsChartProps) {
  if (!data || data.length === 0) {
    return (
      <div className="flex items-center justify-center h-[350px] text-muted-foreground">
        No failure data available
      </div>
    );
  }

  return (
    <ResponsiveContainer width="100%" height={350}>
      <BarChart 
        data={data} 
        layout="vertical" 
        margin={{ top: 5, right: 30, left: 10, bottom: 5 }}
      >
        <CartesianGrid strokeDasharray="3 3" horizontal={false} stroke="var(--graphite)" opacity={0.3} />
        <XAxis type="number" hide />
        <YAxis 
          dataKey="atom_name" 
          type="category" 
          stroke="var(--text-3)"
          fontSize={11}
          tickLine={false}
          axisLine={false}
          width={100}
        />
        <Tooltip 
          cursor={{ fill: 'var(--graphite)', opacity: 0.1 }}
          contentStyle={{ 
            backgroundColor: 'var(--midnight)',
            borderColor: 'var(--graphite)',
            color: 'var(--text-1)',
            borderRadius: '8px',
          }}
          itemStyle={{ fontSize: '12px' }}
          formatter={(value, _name, props) => [value, `Failures (${props.payload.alias})`]}
        />
        <Bar 
          dataKey="failure_count" 
          fill="var(--danger)" 
          radius={[0, 4, 4, 0]} 
          barSize={20}
        >
          {data.map((_, index) => (
            <Cell key={`cell-${index}`} opacity={1 - index * 0.15} />
          ))}
        </Bar>
      </BarChart>
    </ResponsiveContainer>
  );
}
