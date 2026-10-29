import { Button as AriaButton, type ButtonProps } from "react-aria-components";

type Variant = "primary" | "secondary" | "danger" | "quiet" | "quiet-danger";

interface Props extends ButtonProps {
  variant?: Variant;
}

/** The button look for a link, such as a download or a page to open, so it matches the other buttons. */
export function buttonClass(variant: Variant = "primary"): string {
  return `samuori-button samuori-button--${variant} no-underline`;
}

export function Button({ variant = "primary", ...props }: Props) {
  return (
    <AriaButton
      {...props}
      className={`samuori-button samuori-button--${variant}`}
    />
  );
}
