import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { events } from "@/lib/events";

export function useDataAssertionsEnabled() {
  return (
    useQuery({
      queryKey: ["system-features"],
      queryFn: api.getSystemFeatures,
      staleTime: 60_000,
    }).data?.data_assertions_enabled === true
  );
}

export function useHoldInvalidation(enabled: boolean) {
  const client = useQueryClient();
  useEffect(() => {
    if (!enabled) return;
    const invalidate = () => {
      void client.invalidateQueries({ queryKey: ["datasets"] });
      void client.invalidateQueries({ queryKey: ["dataset-holds"] });
      void client.invalidateQueries({ queryKey: ["lineage"] });
    };
    const types = ["dataset_held", "dataset_released", "run_held_upstream"];
    types.forEach((type) => events.subscribe(type, invalidate));
    return () => types.forEach((type) => events.unsubscribe(type, invalidate));
  }, [client, enabled]);
}
