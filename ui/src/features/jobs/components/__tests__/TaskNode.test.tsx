import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { ReactFlowProvider } from 'reactflow';
import { TaskNode } from '../TaskNode';
import type { NodeProps } from 'reactflow';

function renderTaskNode(data: Record<string, unknown>) {
  const props = {
    id: 'test-node',
    data,
    type: 'task',
    selected: false,
    zIndex: 0,
    isConnectable: true,
    xPos: 0,
    yPos: 0,
    dragging: false,
  } as unknown as NodeProps;

  return render(
    <ReactFlowProvider>
      <TaskNode {...props} />
    </ReactFlowProvider>
  );
}

describe('TaskNode', () => {
  it('renders task with succeeded status', () => {
    renderTaskNode({
      label: 'task-abc123',
      status: 'succeeded',
      atom: { image: 'alpine:latest', engine: 'docker', command: ['echo', 'hello'] },
      engine: 'docker',
      command: ['echo', 'hello'],
    });
    // The status icon should be a CheckCircle2 (green check)
    const svgs = document.querySelectorAll('.text-green-400');
    expect(svgs.length).toBeGreaterThan(0);
  });

  it('renders task with failed status and shows error info', () => {
    renderTaskNode({
      label: 'task-def456',
      status: 'failed',
      atom: { image: 'node:18', engine: 'docker', command: ['npm', 'test'] },
      engine: 'docker',
      command: ['npm', 'test'],
      error: 'exit code 1',
    });
    expect(screen.getByText('exit code 1')).toBeInTheDocument();
    expect(screen.getByText('Error Details')).toBeInTheDocument();
  });

  it('renders docker engine icon', () => {
    renderTaskNode({
      label: 'task-ghi789',
      status: 'pending',
      atom: { image: 'alpine:latest', engine: 'docker', command: [] },
      engine: 'docker',
      command: [],
    });
    // Container icon should render for docker engine
    const engineIcons = document.querySelectorAll('.text-blue-400');
    expect(engineIcons.length).toBeGreaterThan(0);
  });

  it('renders command arguments', () => {
    renderTaskNode({
      label: 'task-jkl012',
      status: 'pending',
      atom: { image: 'python:3', engine: 'docker', command: ['python', '-m', 'pytest'] },
      engine: 'docker',
      command: ['python', '-m', 'pytest'],
    });
    expect(screen.getByText('python')).toBeInTheDocument();
    expect(screen.getByText('-m')).toBeInTheDocument();
    expect(screen.getByText('pytest')).toBeInTheDocument();
  });

  it('renders image name', () => {
    renderTaskNode({
      label: 'task-mno345',
      status: 'pending',
      atom: { image: 'registry.example.com/myorg/myimage:v2', engine: 'docker', command: [] },
      engine: 'docker',
      command: [],
    });
    expect(screen.getByText('myimage:v2')).toBeInTheDocument();
  });
});
