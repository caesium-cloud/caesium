import { memo } from "react";
import type { NodeProps } from "reactflow";
import { TaskNode } from "./TaskNode";

export const BranchNode = memo((props: NodeProps) => <TaskNode {...props} data={{ ...props.data, taskType: "branch" }} />);
BranchNode.displayName = "BranchNode";
