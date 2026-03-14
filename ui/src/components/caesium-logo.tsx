import { cn } from "@/lib/utils";

interface CaesiumLogoProps {
  className?: string;
}

export function CaesiumLogo({ className }: CaesiumLogoProps) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 512 512"
      fill="none"
      className={cn("text-primary", className)}
    >
      <circle cx="256" cy="256" r="60" fill="currentColor" opacity="0.15" />
      <ellipse
        cx="256" cy="256" rx="210" ry="70"
        stroke="currentColor" strokeWidth="3.5" opacity="0.85"
        transform="rotate(-60 256 256)"
      />
      <ellipse
        cx="256" cy="256" rx="210" ry="70"
        stroke="currentColor" strokeWidth="3.5" opacity="0.85"
      />
      <ellipse
        cx="256" cy="256" rx="210" ry="70"
        stroke="currentColor" strokeWidth="3.5" opacity="0.85"
        transform="rotate(60 256 256)"
      />
      <circle cx="256" cy="256" r="18" fill="currentColor" />
      <circle cx="361" cy="74" r="9" className="fill-caesium-gold" />
      <circle cx="46" cy="256" r="9" className="fill-caesium-gold" />
      <circle cx="361" cy="438" r="9" className="fill-caesium-gold" />
    </svg>
  );
}
