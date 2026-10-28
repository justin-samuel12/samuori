import { Button as AriaButton, type ButtonProps } from "react-aria-components";

interface Props extends ButtonProps {
  variant?: "primary" | "secondary" | "danger" | "quiet";
}

export function Button({ variant = "primary", ...props }: Props) {
  return (
    <AriaButton
      {...props}
      className={`samuori-button samuori-button--${variant}`}
    />
  );
}
