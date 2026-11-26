package com.acme.config;

import com.acme.order.OrderFacade;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

@Configuration
public class OrderConfig {
    @Bean
    public OrderFacade orderFacade() {
        return new OrderFacade(3);
    }
}
